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
