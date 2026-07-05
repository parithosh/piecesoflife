package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DefaultQuestion is a prompt stitched into every new issue. The enabled
// flag is the global switch; per-issue copies live in questions with
// source 'default' and are edited or removed there like any other question.
type DefaultQuestion struct {
	ID        int64     `json:"id"`
	Text      string    `json:"text"`
	Enabled   bool      `json:"enabled"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
}

// ListDefaultQuestions returns every default question, enabled or not.
func (s *Store) ListDefaultQuestions(ctx context.Context) ([]DefaultQuestion, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, text, enabled, sort_order, created_at
		 FROM default_questions ORDER BY sort_order, id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing default questions: %w", err)
	}
	defer rows.Close()

	questions := make([]DefaultQuestion, 0, 4)

	for rows.Next() {
		var q DefaultQuestion

		if err := rows.Scan(
			&q.ID, &q.Text, &q.Enabled, &q.SortOrder, &q.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning default question: %w", err)
		}

		questions = append(questions, q)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating default questions: %w", err)
	}

	return questions, nil
}

// ListEnabledDefaultQuestions returns the default questions that should be
// added to new issues.
func (s *Store) ListEnabledDefaultQuestions(ctx context.Context) ([]DefaultQuestion, error) {
	all, err := s.ListDefaultQuestions(ctx)
	if err != nil {
		return nil, err
	}

	enabled := make([]DefaultQuestion, 0, len(all))

	for _, q := range all {
		if q.Enabled {
			enabled = append(enabled, q)
		}
	}

	return enabled, nil
}

// CreateDefaultQuestion adds a custom default prompt at the end of the
// list, enabled, and returns its ID. Text is UNIQUE — inserting a duplicate
// returns an error.
func (s *Store) CreateDefaultQuestion(
	ctx context.Context, text string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO default_questions (text, enabled, sort_order)
		 VALUES (?, 1, (SELECT COALESCE(MAX(sort_order) + 1, 0) FROM default_questions))`,
		text,
	)
	if err != nil {
		return 0, fmt.Errorf("creating default question: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new default question id: %w", err)
	}

	return id, nil
}

// UpdateDefaultQuestionText rewords a default question for all future
// issues. Copies already landed on issues keep the text they were sent with.
func (s *Store) UpdateDefaultQuestionText(
	ctx context.Context, id int64, text string,
) error {
	res, err := s.write.ExecContext(ctx,
		"UPDATE default_questions SET text = ? WHERE id = ?", text, id,
	)
	if err != nil {
		return fmt.Errorf("updating default question %d text: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("default question %d: %w", id, sql.ErrNoRows)
	}

	return nil
}

// DeleteDefaultQuestion removes a default prompt from all future issues.
// Copies already landed on issues are ordinary questions rows and survive.
func (s *Store) DeleteDefaultQuestion(ctx context.Context, id int64) error {
	res, err := s.write.ExecContext(ctx,
		"DELETE FROM default_questions WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting default question %d: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("default question %d: %w", id, sql.ErrNoRows)
	}

	return nil
}

// ReorderDefaultQuestions rewrites sort_order to match the given ID order.
// The IDs must cover the current set exactly — a stale list (another admin
// added or removed a question meanwhile) is rejected whole.
func (s *Store) ReorderDefaultQuestions(ctx context.Context, ids []int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning default question reorder: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM default_questions",
	).Scan(&count); err != nil {
		return fmt.Errorf("counting default questions: %w", err)
	}

	if count != len(ids) {
		return fmt.Errorf("reordering default questions (%d ids for %d questions): %w",
			len(ids), count, ErrOrderMismatch)
	}

	for i, id := range ids {
		res, err := tx.ExecContext(ctx,
			"UPDATE default_questions SET sort_order = ? WHERE id = ?", i, id,
		)
		if err != nil {
			return fmt.Errorf("reordering default question %d: %w", id, err)
		}

		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("default question %d: %w", id, ErrOrderMismatch)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing default question reorder: %w", err)
	}

	return nil
}

// SetDefaultQuestionEnabled flips the global switch for one default question.
func (s *Store) SetDefaultQuestionEnabled(
	ctx context.Context, id int64, enabled bool,
) error {
	res, err := s.write.ExecContext(ctx,
		"UPDATE default_questions SET enabled = ? WHERE id = ?", enabled, id,
	)
	if err != nil {
		return fmt.Errorf("updating default question %d: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("default question %d: %w", id, sql.ErrNoRows)
	}

	return nil
}

// SetAllDefaultQuestionsEnabled flips the global switch for every default
// question at once.
func (s *Store) SetAllDefaultQuestionsEnabled(
	ctx context.Context, enabled bool,
) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE default_questions SET enabled = ?", enabled,
	)
	if err != nil {
		return fmt.Errorf("updating all default questions: %w", err)
	}

	return nil
}
