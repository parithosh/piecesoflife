package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Question represents a question assigned to an issue.
type Question struct {
	ID          int64     `json:"id"`
	IssueID     int64     `json:"issue_id"`
	Text        string    `json:"text"`
	Category    *string   `json:"category"`
	Source      string    `json:"source"`
	SubmittedBy *int64    `json:"submitted_by"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

// QuestionBank represents a pre-generated question in the bank.
type QuestionBank struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Category  string    `json:"category"`
	Used      bool      `json:"used"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateQuestion inserts a question for an issue and returns its ID.
func (s *Store) CreateQuestion(
	ctx context.Context,
	issueID int64, text string, category *string,
	source string, submittedBy *int64, sortOrder int,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO questions
		 (issue_id, text, category, source, submitted_by, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		issueID, text, category, source, submittedBy, sortOrder,
	)
	if err != nil {
		return 0, fmt.Errorf("creating question: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new question id: %w", err)
	}

	return id, nil
}

// GetQuestionByID returns a question by its database ID.
func (s *Store) GetQuestionByID(
	ctx context.Context, id int64,
) (*Question, error) {
	var q Question

	err := s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, text, category, source,
		        submitted_by, sort_order, created_at
		 FROM questions WHERE id = ?`, id,
	).Scan(&q.ID, &q.IssueID, &q.Text, &q.Category,
		&q.Source, &q.SubmittedBy, &q.SortOrder, &q.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting question %d: %w", id, err)
	}

	return &q, nil
}

// ListQuestionsByIssue returns all questions for an issue.
func (s *Store) ListQuestionsByIssue(
	ctx context.Context, issueID int64,
) ([]Question, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, issue_id, text, category, source,
		        submitted_by, sort_order, created_at
		 FROM questions WHERE issue_id = ?
		 ORDER BY sort_order`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing questions for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	var questions []Question

	for rows.Next() {
		var q Question

		err := rows.Scan(&q.ID, &q.IssueID, &q.Text, &q.Category,
			&q.Source, &q.SubmittedBy, &q.SortOrder, &q.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning question: %w", err)
		}

		questions = append(questions, q)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating questions: %w", err)
	}

	if questions == nil {
		questions = make([]Question, 0)
	}

	return questions, nil
}

// UpdateQuestion changes a question's text.
func (s *Store) UpdateQuestion(
	ctx context.Context, id int64, text string,
) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE questions SET text = ? WHERE id = ?", text, id,
	)
	if err != nil {
		return fmt.Errorf("updating question %d: %w", id, err)
	}

	return nil
}

// DeleteQuestion removes a question by ID.
func (s *Store) DeleteQuestion(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM questions WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting question %d: %w", id, err)
	}

	return nil
}

// DeleteQuestionsByIssue removes every question belonging to an issue.
// Used by onboarding retry to rewrite the question set without creating
// duplicates. Safe only when no responses exist for those questions
// (foreign key references without ON DELETE CASCADE).
func (s *Store) DeleteQuestionsByIssue(
	ctx context.Context, issueID int64,
) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM questions WHERE issue_id = ?", issueID,
	)
	if err != nil {
		return fmt.Errorf("deleting questions for issue %d: %w", issueID, err)
	}

	return nil
}

// SelectRandomUnusedQuestions picks questions from the bank with category variety.
func (s *Store) SelectRandomUnusedQuestions(
	ctx context.Context, count int,
) ([]QuestionBank, error) {
	categories := []string{
		"life_updates", "deep_thoughts", "fun_silly",
		"memories", "goals", "recommendations", "hypotheticals",
	}

	results := make([]QuestionBank, 0, count)
	usedIDs := make(map[int64]bool, count)

	// Round 1: one unused question per category.
	for _, cat := range categories {
		if len(results) >= count {
			break
		}

		var q QuestionBank

		err := s.read.QueryRowContext(ctx,
			`SELECT id, text, category, used, created_at
			 FROM question_bank WHERE used = 0 AND category = ?
			 ORDER BY RANDOM() LIMIT 1`, cat,
		).Scan(&q.ID, &q.Text, &q.Category, &q.Used, &q.CreatedAt)
		if err == sql.ErrNoRows {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("selecting question from %s: %w", cat, err)
		}

		results = append(results, q)
		usedIDs[q.ID] = true
	}

	// Round 2: fill remaining from any unused questions.
	if len(results) < count {
		remaining := count - len(results)

		rows, err := s.read.QueryContext(ctx,
			`SELECT id, text, category, used, created_at
			 FROM question_bank WHERE used = 0
			 ORDER BY RANDOM() LIMIT ?`, remaining+len(usedIDs),
		)
		if err != nil {
			return nil, fmt.Errorf("selecting remaining questions: %w", err)
		}
		defer rows.Close()

		for rows.Next() && len(results) < count {
			var q QuestionBank

			if err := rows.Scan(
				&q.ID, &q.Text, &q.Category, &q.Used, &q.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning question: %w", err)
			}

			if !usedIDs[q.ID] {
				results = append(results, q)
				usedIDs[q.ID] = true
			}
		}

		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating remaining questions: %w", err)
		}
	}

	// Round 3: if still under count, fill with any (including used).
	if len(results) < count {
		remaining := count - len(results)

		rows, err := s.read.QueryContext(ctx,
			`SELECT id, text, category, used, created_at
			 FROM question_bank ORDER BY RANDOM() LIMIT ?`,
			remaining+len(usedIDs),
		)
		if err != nil {
			return nil, fmt.Errorf("selecting fallback questions: %w", err)
		}
		defer rows.Close()

		for rows.Next() && len(results) < count {
			var q QuestionBank

			if err := rows.Scan(
				&q.ID, &q.Text, &q.Category, &q.Used, &q.CreatedAt,
			); err != nil {
				return nil, fmt.Errorf("scanning fallback question: %w", err)
			}

			if !usedIDs[q.ID] {
				results = append(results, q)
			}
		}

		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating fallback questions: %w", err)
		}
	}

	return results, nil
}

// MarkBankQuestionUsed marks a question bank entry as used.
func (s *Store) MarkBankQuestionUsed(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE question_bank SET used = 1 WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("marking bank question %d as used: %w", id, err)
	}

	return nil
}

// ListQuestionBank returns paginated question bank entries.
func (s *Store) ListQuestionBank(
	ctx context.Context,
	category *string, used *bool,
	page, perPage int,
) ([]QuestionBank, int, error) {
	where := "WHERE 1=1"
	args := make([]any, 0, 4)

	if category != nil {
		where += " AND category = ?"
		args = append(args, *category)
	}

	if used != nil {
		where += " AND used = ?"
		args = append(args, *used)
	}

	var total int

	countArgs := make([]any, len(args))
	copy(countArgs, args)

	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM question_bank "+where, countArgs...,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting question bank: %w", err)
	}

	offset := (page - 1) * perPage
	args = append(args, perPage, offset)

	rows, err := s.read.QueryContext(ctx,
		`SELECT id, text, category, used, created_at
		 FROM question_bank `+where+
			` ORDER BY category, id LIMIT ? OFFSET ?`, args...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("listing question bank: %w", err)
	}
	defer rows.Close()

	var questions []QuestionBank

	for rows.Next() {
		var q QuestionBank

		if err := rows.Scan(
			&q.ID, &q.Text, &q.Category, &q.Used, &q.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning bank question: %w", err)
		}

		questions = append(questions, q)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating bank questions: %w", err)
	}

	if questions == nil {
		questions = make([]QuestionBank, 0)
	}

	return questions, total, nil
}

// CreateBankQuestion adds a new question to the bank.
func (s *Store) CreateBankQuestion(
	ctx context.Context, text, category string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO question_bank (text, category) VALUES (?, ?)`,
		text, category,
	)
	if err != nil {
		return 0, fmt.Errorf("creating bank question: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new bank question id: %w", err)
	}

	return id, nil
}

// UpdateBankQuestion modifies a question bank entry.
func (s *Store) UpdateBankQuestion(
	ctx context.Context, id int64, text, category string,
) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE question_bank SET text = ?, category = ? WHERE id = ?",
		text, category, id,
	)
	if err != nil {
		return fmt.Errorf("updating bank question %d: %w", id, err)
	}

	return nil
}

// DeleteBankQuestion removes a question from the bank.
func (s *Store) DeleteBankQuestion(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM question_bank WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting bank question %d: %w", id, err)
	}

	return nil
}

// CountResponsesByQuestion returns, for each question of an issue, how many
// response rows exist (drafts included — a draft goes just as stale as a
// submitted answer if the question changes underneath it). Questions with no
// responses are present with a zero count.
func (s *Store) CountResponsesByQuestion(
	ctx context.Context, issueID int64,
) (map[int64]int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT q.id, COUNT(r.id)
		 FROM questions q
		 LEFT JOIN responses r ON r.question_id = q.id
		 WHERE q.issue_id = ?
		 GROUP BY q.id`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("counting responses by question for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	counts := make(map[int64]int)

	for rows.Next() {
		var qID int64
		var n int
		if err := rows.Scan(&qID, &n); err != nil {
			return nil, fmt.Errorf("scanning question response count: %w", err)
		}

		counts[qID] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating question response counts: %w", err)
	}

	return counts, nil
}
