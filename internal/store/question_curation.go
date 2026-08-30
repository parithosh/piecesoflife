package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrIssueQuestionsNotCuratable means the requested issue is not an
	// upcoming draft owned by the selected Loop.
	ErrIssueQuestionsNotCuratable = errors.New("issue questions are not curatable")
	// ErrIssueNotAcceptingSuggestions means the draft opened or its admin
	// froze the question set before the suggestion could be stored.
	ErrIssueNotAcceptingSuggestions = errors.New("issue is not accepting suggestions")
	questionBankCategories          = [...]string{
		"life_updates", "deep_thoughts", "fun_silly", "memories",
		"goals", "recommendations", "hypotheticals",
	}
)

type questionBankQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PrepareDraftQuestions atomically turns an upcoming draft's existing member
// suggestions into a complete, admin-curated question set. Enabled defaults
// follow the suggestions and bank questions fill any remaining target slots.
// Repeating the operation after it succeeds is an idempotent no-op.
func (s *Store) PrepareDraftQuestions(
	ctx context.Context, groupID, issueID int64, target int,
) error {
	if target <= 0 {
		return fmt.Errorf("preparing issue %d questions: target must be positive", issueID)
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning issue %d question preparation: %w", issueID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status    string
		opensAt   time.Time
		curatedAt sql.NullTime
	)

	err = tx.QueryRowContext(ctx,
		`SELECT status, opens_at, questions_curated_at
		 FROM issues WHERE id = ? AND group_id = ?`,
		issueID, groupID,
	).Scan(&status, &opensAt, &curatedAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("preparing issue %d questions: %w",
			issueID, ErrIssueQuestionsNotCuratable)
	}
	if err != nil {
		return fmt.Errorf("loading issue %d for question preparation: %w", issueID, err)
	}

	if curatedAt.Valid {
		return nil
	}

	now := time.Now().UTC()
	if status != "draft" || !opensAt.After(now) {
		return fmt.Errorf("preparing issue %d questions: %w",
			issueID, ErrIssueQuestionsNotCuratable)
	}

	var (
		questionCount int
		hasDefaults   bool
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), EXISTS(
		     SELECT 1 FROM questions WHERE issue_id = ? AND source = 'default'
		 )
		 FROM questions WHERE issue_id = ?`,
		issueID, issueID,
	).Scan(&questionCount, &hasDefaults); err != nil {
		return fmt.Errorf("counting issue %d questions: %w", issueID, err)
	}

	nextOrder := questionCount
	if !hasDefaults {
		rows, err := tx.QueryContext(ctx,
			`SELECT text FROM default_questions
			 WHERE group_id = ? AND enabled = 1
			 ORDER BY sort_order, id`, groupID,
		)
		if err != nil {
			return fmt.Errorf("listing enabled defaults for issue %d: %w", issueID, err)
		}

		defaults := make([]string, 0, 4)
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scanning default for issue %d: %w", issueID, err)
			}
			defaults = append(defaults, text)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterating defaults for issue %d: %w", issueID, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("closing defaults for issue %d: %w", issueID, err)
		}

		for _, text := range defaults {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO questions (issue_id, text, source, sort_order)
				 VALUES (?, ?, 'default', ?)`,
				issueID, text, nextOrder,
			); err != nil {
				return fmt.Errorf("adding default to issue %d: %w", issueID, err)
			}
			nextOrder++
		}
	}

	if need := target - nextOrder; need > 0 {
		bankQuestions, err := selectRandomUnusedQuestions(ctx, tx, groupID, need)
		if err != nil {
			return fmt.Errorf("selecting bank questions for issue %d: %w", issueID, err)
		}

		for _, question := range bankQuestions {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO questions
				 (issue_id, text, category, source, sort_order)
				 VALUES (?, ?, ?, 'bank', ?)`,
				issueID, question.Text, question.Category, nextOrder,
			); err != nil {
				return fmt.Errorf("adding bank question %d to issue %d: %w",
					question.ID, issueID, err)
			}
			nextOrder++

			if _, err := tx.ExecContext(ctx,
				"UPDATE question_bank SET used = 1 WHERE id = ?", question.ID,
			); err != nil {
				return fmt.Errorf("marking bank question %d used: %w", question.ID, err)
			}
		}
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE issues SET questions_curated_at = ?
		 WHERE id = ? AND group_id = ? AND status = 'draft'
		   AND opens_at > ? AND questions_curated_at IS NULL`,
		now, issueID, groupID, now,
	)
	if err != nil {
		return fmt.Errorf("marking issue %d questions curated: %w", issueID, err)
	}

	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking issue %d question curation: %w", issueID, err)
	}
	if updated != 1 {
		return fmt.Errorf("preparing issue %d questions: %w",
			issueID, ErrIssueQuestionsNotCuratable)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing issue %d question preparation: %w", issueID, err)
	}

	return nil
}

// CreateSuggestedQuestion inserts a member suggestion only while the issue is
// an upcoming, uncurated draft. The state predicate and insert share one SQL
// statement so curation cannot race a late, unreviewed suggestion into the set.
func (s *Store) CreateSuggestedQuestion(
	ctx context.Context, groupID, issueID, userID int64,
	text string, category *string,
) (int64, error) {
	now := time.Now().UTC()
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO questions
		 (issue_id, text, category, source, submitted_by, sort_order)
		 SELECT i.id, ?, ?, 'friend', ?,
		        COALESCE((SELECT MAX(q.sort_order) + 1
		                  FROM questions q WHERE q.issue_id = i.id), 0)
		 FROM issues i
		 WHERE i.id = ? AND i.group_id = ? AND i.status = 'draft'
		   AND i.opens_at > ? AND i.questions_curated_at IS NULL`,
		text, category, userID, issueID, groupID, now,
	)
	if err != nil {
		return 0, fmt.Errorf("creating suggested question: %w", err)
	}

	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("checking suggested question insert: %w", err)
	}
	if inserted != 1 {
		return 0, ErrIssueNotAcceptingSuggestions
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting suggested question id: %w", err)
	}

	return id, nil
}

func selectRandomUnusedQuestions(
	ctx context.Context, query questionBankQuerier, groupID int64, count int,
) ([]QuestionBank, error) {
	if count <= 0 {
		return make([]QuestionBank, 0), nil
	}

	results := make([]QuestionBank, 0, count)
	usedIDs := make(map[int64]struct{}, count)

	for _, category := range questionBankCategories {
		if len(results) >= count {
			break
		}

		var question QuestionBank
		err := query.QueryRowContext(ctx,
			`SELECT id, group_id, text, category, used, created_at
			 FROM question_bank
			 WHERE group_id = ? AND used = 0 AND category = ?
			 ORDER BY RANDOM() LIMIT 1`, groupID, category,
		).Scan(
			&question.ID, &question.GroupID, &question.Text, &question.Category,
			&question.Used, &question.CreatedAt,
		)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("selecting question from %s: %w", category, err)
		}

		results = append(results, question)
		usedIDs[question.ID] = struct{}{}
	}

	if len(results) < count {
		remaining := count - len(results)
		rows, err := query.QueryContext(ctx,
			`SELECT id, group_id, text, category, used, created_at
			 FROM question_bank WHERE group_id = ? AND used = 0
			 ORDER BY RANDOM() LIMIT ?`, groupID, remaining+len(usedIDs),
		)
		if err != nil {
			return nil, fmt.Errorf("selecting remaining questions: %w", err)
		}
		defer rows.Close()

		for rows.Next() && len(results) < count {
			var question QuestionBank
			if err := rows.Scan(
				&question.ID, &question.GroupID, &question.Text, &question.Category,
				&question.Used, &question.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning remaining question: %w", err)
			}

			if _, seen := usedIDs[question.ID]; !seen {
				results = append(results, question)
				usedIDs[question.ID] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating remaining questions: %w", err)
		}
	}

	if len(results) < count {
		remaining := count - len(results)
		rows, err := query.QueryContext(ctx,
			`SELECT id, group_id, text, category, used, created_at
			 FROM question_bank WHERE group_id = ?
			 ORDER BY RANDOM() LIMIT ?`, groupID, remaining+len(usedIDs),
		)
		if err != nil {
			return nil, fmt.Errorf("selecting fallback questions: %w", err)
		}
		defer rows.Close()

		for rows.Next() && len(results) < count {
			var question QuestionBank
			if err := rows.Scan(
				&question.ID, &question.GroupID, &question.Text, &question.Category,
				&question.Used, &question.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning fallback question: %w", err)
			}

			if _, seen := usedIDs[question.ID]; !seen {
				results = append(results, question)
				usedIDs[question.ID] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating fallback questions: %w", err)
		}
	}

	return results, nil
}
