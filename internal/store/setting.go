package store

import (
	"context"
	"fmt"
	"time"
)

// Settings holds the application-wide configuration (single-row table).
type Settings struct {
	ID                   int64      `json:"id"`
	LoopName             string     `json:"loop_name"`
	Tagline              *string    `json:"tagline"`
	Frequency            string     `json:"frequency"`
	SubmissionWindowDays int        `json:"submission_window_days"`
	StartDatetime        *time.Time `json:"start_datetime"`
	Timezone             string     `json:"timezone"`
	InviteNote           *string    `json:"invite_note"`
	SetupComplete        bool       `json:"setup_complete"`
	AccentColor          string     `json:"accent_color"`
	AutoCreateEnabled    bool       `json:"auto_create_enabled"`
	AllowPublicMementos  bool       `json:"allow_public_mementos"`
	QuestionsPerIssue    int        `json:"questions_per_issue"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// GetSettings returns the single settings row.
func (s *Store) GetSettings(ctx context.Context) (*Settings, error) {
	var st Settings

	err := s.read.QueryRowContext(ctx,
		`SELECT id, loop_name, tagline, frequency,
		        submission_window_days, start_datetime, timezone,
		        invite_note, setup_complete,
		        accent_color, auto_create_enabled, allow_public_mementos,
		        questions_per_issue, created_at, updated_at
		 FROM settings WHERE id = 1`,
	).Scan(&st.ID, &st.LoopName, &st.Tagline, &st.Frequency,
		&st.SubmissionWindowDays, &st.StartDatetime, &st.Timezone,
		&st.InviteNote, &st.SetupComplete,
		&st.AccentColor, &st.AutoCreateEnabled, &st.AllowPublicMementos,
		&st.QuestionsPerIssue, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting settings: %w", err)
	}

	return &st, nil
}

// UpdateSettings writes all settings fields.
func (s *Store) UpdateSettings(ctx context.Context, st *Settings) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE settings SET
			loop_name = ?, tagline = ?, frequency = ?,
			submission_window_days = ?, start_datetime = ?,
			timezone = ?, invite_note = ?,
			accent_color = ?, auto_create_enabled = ?,
			allow_public_mementos = ?, questions_per_issue = ?,
			updated_at = CURRENT_TIMESTAMP
		 WHERE id = 1`,
		st.LoopName, st.Tagline, st.Frequency,
		st.SubmissionWindowDays, st.StartDatetime,
		st.Timezone, st.InviteNote,
		st.AccentColor, st.AutoCreateEnabled,
		st.AllowPublicMementos, st.QuestionsPerIssue,
	)
	if err != nil {
		return fmt.Errorf("updating settings: %w", err)
	}

	return nil
}

// CompleteSetup marks the onboarding wizard as finished.
func (s *Store) CompleteSetup(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE settings SET setup_complete = 1,
		 updated_at = CURRENT_TIMESTAMP WHERE id = 1`,
	)
	if err != nil {
		return fmt.Errorf("completing setup: %w", err)
	}

	return nil
}

// IsSetupComplete returns whether the onboarding wizard has been completed.
func (s *Store) IsSetupComplete(ctx context.Context) (bool, error) {
	var complete bool

	err := s.read.QueryRowContext(ctx,
		"SELECT setup_complete FROM settings WHERE id = 1",
	).Scan(&complete)
	if err != nil {
		return false, fmt.Errorf("checking setup status: %w", err)
	}

	return complete, nil
}
