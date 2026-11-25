package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Errors from AnswerTogether and AnswerSeparately.
var (
	// ErrQuestionNotYours: the question is not a hot-seat question for this
	// member in this Loop.
	ErrQuestionNotYours = errors.New("not your hot-seat question")
	// ErrNotCollecting: the round no longer accepts answers.
	ErrNotCollecting = errors.New("issue is not collecting")
	// ErrStaleGrouping: the questions are not grouped the way the caller's
	// page showed them (another tab or device changed it).
	ErrStaleGrouping = errors.New("question grouping changed")
	// ErrBothAnswered: each question already has its own answer; v1 never
	// merges two answers.
	ErrBothAnswered = errors.New("more than one question already has an answer")
)

// hotSeatQuestion is what AnswerTogether needs to know about one question.
type hotSeatQuestion struct {
	id, issueID, sortOrder int64
	answeredWith           sql.NullInt64
	responseID             sql.NullInt64
	isDraft                bool
	hasContent             bool
}

// loadHotSeatQuestion reads a question inside tx, refusing anything that is
// not a hot-seat question for userID in a collecting issue of groupID.
func loadHotSeatQuestion(
	ctx context.Context, tx *sql.Tx, groupID, userID, questionID int64,
) (*hotSeatQuestion, error) {
	var q hotSeatQuestion
	var status string

	err := tx.QueryRowContext(ctx,
		`SELECT q.id, q.issue_id, q.sort_order, q.answered_with, i.status,
		        r.id, COALESCE(r.is_draft, 1)
		 FROM questions q
		 JOIN issues i ON i.id = q.issue_id
		 LEFT JOIN responses r ON r.question_id = q.id AND r.user_id = ?
		 WHERE q.id = ? AND q.target_user_id = ? AND i.group_id = ?`,
		userID, questionID, userID, groupID,
	).Scan(&q.id, &q.issueID, &q.sortOrder, &q.answeredWith, &status,
		&q.responseID, &q.isDraft)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrQuestionNotYours
	}
	if err != nil {
		return nil, fmt.Errorf("loading question %d: %w", questionID, err)
	}
	if status != "collecting" {
		return nil, ErrNotCollecting
	}

	// "Answered" means the same here as on every page: HasAnswerContent.
	if q.responseID.Valid {
		rows, err := tx.QueryContext(ctx,
			`SELECT type, content, file_path, link_url
			 FROM response_blocks WHERE response_id = ?`, q.responseID.Int64)
		if err != nil {
			return nil, fmt.Errorf("loading answer blocks: %w", err)
		}
		defer rows.Close()
		var blocks []ResponseBlock
		for rows.Next() {
			var b ResponseBlock
			if err := rows.Scan(&b.Type, &b.Content, &b.FilePath, &b.LinkURL); err != nil {
				return nil, fmt.Errorf("scanning answer block: %w", err)
			}
			blocks = append(blocks, b)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating answer blocks: %w", err)
		}
		q.hasContent = HasAnswerContent(blocks)
	}

	return &q, nil
}

// AnswerTogether lets a member answer several of their hot-seat questions
// with one answer. Every question must still be a root, as the member's page
// showed it (a group the page showed is picked by its root). The primary is
// the one that already has content; with none, the one earliest in the
// issue. Answers are never merged: when two have content it returns
// ErrBothAnswered. The others' responses then hold no content (Submit creates
// blank rows for untouched cards) and are deleted. When the primary has no
// response but another had one, the primary gets a fresh blank one, under a
// new ID so a stale tab still writing to an old one gets a 404 instead of
// writing into the primary, and submitted if any of them was so the member's
// Done status is unchanged. The primary's own response is never changed:
// grouping is not consent to submit a draft. Questions already joined to the
// others move to the primary. Returns the primary's ID.
func (s *Store) AnswerTogether(
	ctx context.Context, groupID, userID int64, questionIDs []int64,
) (int64, error) {
	if len(questionIDs) < 2 {
		return 0, ErrStaleGrouping
	}
	if err := checkUniqueIDs(questionIDs); err != nil {
		return 0, ErrStaleGrouping
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning answer-together transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	qs := make([]*hotSeatQuestion, 0, len(questionIDs))
	for _, id := range questionIDs {
		q, err := loadHotSeatQuestion(ctx, tx, groupID, userID, id)
		if err != nil {
			return 0, err
		}
		if len(qs) > 0 && q.issueID != qs[0].issueID {
			return 0, ErrQuestionNotYours
		}
		if q.answeredWith.Valid {
			return 0, ErrStaleGrouping
		}
		qs = append(qs, q)
	}

	var primary *hotSeatQuestion
	for _, q := range qs {
		switch {
		case q.hasContent && primary != nil && primary.hasContent:
			return 0, ErrBothAnswered
		case primary == nil,
			q.hasContent,
			!primary.hasContent && q.sortOrder < primary.sortOrder:
			primary = q
		}
	}

	others := make([]any, 0, len(qs))
	needsRow, submitted := false, false
	for _, q := range qs {
		if q == primary {
			continue
		}
		others = append(others, q.id)
		if !q.responseID.Valid {
			continue
		}
		// Collecting issues have no comments, and a content-free response
		// has no blocks worth keeping (empty text only).
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM responses WHERE id = ?", q.responseID.Int64,
		); err != nil {
			return 0, fmt.Errorf("discarding empty response: %w", err)
		}
		needsRow = !primary.responseID.Valid
		submitted = submitted || !q.isDraft
	}

	if needsRow {
		token, err := newShareToken()
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO responses (user_id, question_id, share_token, is_draft)
			 VALUES (?, ?, ?, ?)`,
			userID, primary.id, token, !submitted,
		); err != nil {
			return 0, fmt.Errorf("giving the primary a blank response: %w", err)
		}
	}

	in := strings.TrimSuffix(strings.Repeat("?,", len(others)), ",")
	args := append([]any{primary.id}, others...)
	args = append(args, others...)
	if _, err := tx.ExecContext(ctx,
		"UPDATE questions SET answered_with = ? WHERE id IN ("+in+") OR answered_with IN ("+in+")",
		args...,
	); err != nil {
		return 0, fmt.Errorf("joining questions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing answer-together: %w", err)
	}

	return primary.id, nil
}

// AnswerSeparately detaches a question from the primary the member's page
// showed it under, giving it its own (empty) answer again. The shared answer
// stays with the primary: this detaches a prompt, it does not split content.
func (s *Store) AnswerSeparately(
	ctx context.Context, groupID, userID, questionID, primaryID int64,
) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning answer-separately transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	q, err := loadHotSeatQuestion(ctx, tx, groupID, userID, questionID)
	if err != nil {
		return err
	}
	if !q.answeredWith.Valid || q.answeredWith.Int64 != primaryID {
		return ErrStaleGrouping
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE questions SET answered_with = NULL WHERE id = ?", questionID,
	); err != nil {
		return fmt.Errorf("separating question %d: %w", questionID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing answer-separately: %w", err)
	}

	return nil
}
