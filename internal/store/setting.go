package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// Theme is the published appearance config (internal/theme.Config
	// JSON); nil means the house look. ThemePrevious is the look it
	// replaced, kept for one-step restore.
	Theme               json.RawMessage `json:"theme,omitempty"`
	ThemePrevious       json.RawMessage `json:"-"`
	AutoCreateEnabled   bool            `json:"auto_create_enabled"`
	AllowPublicMementos bool            `json:"allow_public_mementos"`
	QuestionsPerIssue   int             `json:"questions_per_issue"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// GetSettings returns one group's settings row.
func (s *Store) GetSettings(ctx context.Context, groupID int64) (*Settings, error) {
	var st Settings
	var themeRaw, prevRaw sql.NullString

	err := s.read.QueryRowContext(ctx,
		`SELECT id, group_id, loop_name, tagline, frequency,
		        submission_window_days, start_datetime, timezone,
		        invite_note, setup_complete,
		        theme, theme_previous, auto_create_enabled, allow_public_mementos,
		        questions_per_issue, created_at, updated_at
		 FROM settings WHERE group_id = ?`, groupID,
	).Scan(&st.ID, &st.GroupID, &st.LoopName, &st.Tagline, &st.Frequency,
		&st.SubmissionWindowDays, &st.StartDatetime, &st.Timezone,
		&st.InviteNote, &st.SetupComplete,
		&themeRaw, &prevRaw, &st.AutoCreateEnabled, &st.AllowPublicMementos,
		&st.QuestionsPerIssue, &st.CreatedAt, &st.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting settings for group %d: %w", groupID, err)
	}

	st.Theme = rawJSON(themeRaw)
	st.ThemePrevious = rawJSON(prevRaw)

	return &st, nil
}

func rawJSON(ns sql.NullString) json.RawMessage {
	if !ns.Valid || ns.String == "" {
		return nil
	}

	return json.RawMessage(ns.String)
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}

	return string(raw)
}

// ErrThemeChanged reports that a group's published look changed between
// the caller reading it and trying to replace it.
var ErrThemeChanged = errors.New("theme changed concurrently")

// ErrNoRestorePoint reports a restore with no previous look to go back to.
var ErrNoRestorePoint = errors.New("no previous theme to restore")

// PublishTheme replaces a group's published appearance, but only if it is
// still expected (compare-and-swap; ErrThemeChanged otherwise). The look it
// replaces becomes the restore point, recorded as the JSON literal null
// when that was the house look so "restore the house look" stays distinct
// from "no restore point" (NULL). Re-publishing the current look is a
// no-op that keeps the restore point. nil theme = house look.
func (s *Store) PublishTheme(ctx context.Context, groupID int64, expected, theme json.RawMessage) error {
	if theme != nil && !json.Valid(theme) {
		return fmt.Errorf("theme for group %d is not valid JSON", groupID)
	}

	next := nullJSON(theme)
	result, err := s.write.ExecContext(ctx,
		`UPDATE settings SET
			theme_previous = CASE WHEN theme IS ? THEN theme_previous ELSE COALESCE(theme, 'null') END,
			theme = ?,
			updated_at = CURRENT_TIMESTAMP
		 WHERE group_id = ? AND theme IS ?`,
		next, next, groupID, nullJSON(expected),
	)
	if err != nil {
		return fmt.Errorf("publishing theme for group %d: %w", groupID, err)
	}

	return s.themeRowsAffected(ctx, result, groupID, ErrThemeChanged)
}

// RestoreTheme atomically swaps a group's published look with its restore
// point, so restoring twice is a redo. ErrNoRestorePoint when there is none.
func (s *Store) RestoreTheme(ctx context.Context, groupID int64) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE settings SET
			theme = CASE WHEN theme_previous = 'null' THEN NULL ELSE theme_previous END,
			theme_previous = COALESCE(theme, 'null'),
			updated_at = CURRENT_TIMESTAMP
		 WHERE group_id = ? AND theme_previous IS NOT NULL`,
		groupID,
	)
	if err != nil {
		return fmt.Errorf("restoring theme for group %d: %w", groupID, err)
	}

	return s.themeRowsAffected(ctx, result, groupID, ErrNoRestorePoint)
}

// themeRowsAffected maps a zero-row theme update to ErrNoRows (no such
// group) or the given condition error (row exists, guard failed).
func (s *Store) themeRowsAffected(ctx context.Context, result sql.Result, groupID int64, guardErr error) error {
	n, err := result.RowsAffected()
	if err != nil || n > 0 {
		return err
	}

	var exists bool
	if err := s.read.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM settings WHERE group_id = ?)", groupID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("checking settings for group %d: %w", groupID, err)
	}
	if !exists {
		return fmt.Errorf("settings for group %d: %w", groupID, sql.ErrNoRows)
	}

	return guardErr
}

// UpdateSettings writes all editable settings fields of st's group. A
// GroupID that matches no settings row is an error, never a silent no-op —
// the single-row table this replaced made that impossible by construction.
func (s *Store) UpdateSettings(ctx context.Context, st *Settings) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE settings SET
			loop_name = ?, tagline = ?, frequency = ?,
			submission_window_days = ?, start_datetime = ?,
			timezone = ?, invite_note = ?,
			auto_create_enabled = ?,
			allow_public_mementos = ?, questions_per_issue = ?,
			updated_at = CURRENT_TIMESTAMP
		 WHERE group_id = ?`,
		st.LoopName, st.Tagline, st.Frequency,
		st.SubmissionWindowDays, st.StartDatetime,
		st.Timezone, st.InviteNote,
		st.AutoCreateEnabled,
		st.AllowPublicMementos, st.QuestionsPerIssue,
		st.GroupID,
	)
	if err != nil {
		return fmt.Errorf("updating settings for group %d: %w", st.GroupID, err)
	}

	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("settings for group %d: %w", st.GroupID, sql.ErrNoRows)
	}

	return nil
}

// CompleteSetup marks a group's onboarding wizard as finished. A groupID
// that matches no settings row is an error, never a silent no-op.
func (s *Store) CompleteSetup(ctx context.Context, groupID int64) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE settings SET setup_complete = 1,
		 updated_at = CURRENT_TIMESTAMP WHERE group_id = ?`, groupID,
	)
	if err != nil {
		return fmt.Errorf("completing setup for group %d: %w", groupID, err)
	}

	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("settings for group %d: %w", groupID, sql.ErrNoRows)
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
