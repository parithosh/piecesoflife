package store

import (
	"context"
	"fmt"
	"time"
)

// Settings holds one Loop's configuration (one row per group).
type Settings struct {
	ID                   int64      `json:"id"`
	GroupID              int64      `json:"group_id"`
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

// GetSettings returns one group's settings row.
func (s *Store) GetSettings(ctx context.Context, groupID int64) (*Settings, error) {
	var st Settings

	err := s.read.QueryRowContext(ctx,
		`SELECT id, group_id, loop_name, tagline, frequency,
		        submission_window_days, start_datetime, timezone,
		        invite_note, setup_complete,
		        accent_color, auto_create_enabled, allow_public_mementos,
		        questions_per_issue, created_at, updated_at
		 FROM settings WHERE group_id = ?`, groupID,
	).Scan(&st.ID, &st.GroupID, &st.LoopName, &st.Tagline, &st.Frequency,
		&st.SubmissionWindowDays, &st.StartDatetime, &st.Timezone,
		&st.InviteNote, &st.SetupComplete,
		&st.AccentColor, &st.AutoCreateEnabled, &st.AllowPublicMementos,
		&st.QuestionsPerIssue, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting settings for group %d: %w", groupID, err)
	}

	return &st, nil
}

// UpdateSettings writes all editable settings fields of st's group.
func (s *Store) UpdateSettings(ctx context.Context, st *Settings) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE settings SET
			loop_name = ?, tagline = ?, frequency = ?,
			submission_window_days = ?, start_datetime = ?,
			timezone = ?, invite_note = ?,
			accent_color = ?, auto_create_enabled = ?,
			allow_public_mementos = ?, questions_per_issue = ?,
			updated_at = CURRENT_TIMESTAMP
		 WHERE group_id = ?`,
		st.LoopName, st.Tagline, st.Frequency,
		st.SubmissionWindowDays, st.StartDatetime,
		st.Timezone, st.InviteNote,
		st.AccentColor, st.AutoCreateEnabled,
		st.AllowPublicMementos, st.QuestionsPerIssue,
		st.GroupID,
	)
	if err != nil {
		return fmt.Errorf("updating settings for group %d: %w", st.GroupID, err)
	}

	return nil
}

// CompleteSetup marks a group's onboarding wizard as finished.
func (s *Store) CompleteSetup(ctx context.Context, groupID int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE settings SET setup_complete = 1,
		 updated_at = CURRENT_TIMESTAMP WHERE group_id = ?`, groupID,
	)
	if err != nil {
		return fmt.Errorf("completing setup for group %d: %w", groupID, err)
	}

	return nil
}

// IsSetupComplete reports whether a group's onboarding wizard has finished.
func (s *Store) IsSetupComplete(ctx context.Context, groupID int64) (bool, error) {
	var complete bool

	err := s.read.QueryRowContext(ctx,
		"SELECT setup_complete FROM settings WHERE group_id = ?", groupID,
	).Scan(&complete)
	if err != nil {
		return false, fmt.Errorf("checking setup status for group %d: %w", groupID, err)
	}

	return complete, nil
}
