package store

import (
	"context"
	"fmt"
	"time"
)

// NotificationPreferences holds per-user email notification settings.
type NotificationPreferences struct {
	ID            int64     `json:"id"`
	UserID        int64     `json:"user_id"`
	IssueOpen     bool      `json:"issue_open"`
	Reminders     bool      `json:"reminders"`
	Published     bool      `json:"published"`
	CommentNotify bool      `json:"comment_notify"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// GetNotificationPreferences returns notification preferences for a user.
func (s *Store) GetNotificationPreferences(
	ctx context.Context, userID int64,
) (*NotificationPreferences, error) {
	var np NotificationPreferences

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, issue_open, reminders, published,
		        comment_notify, updated_at
		 FROM notification_preferences WHERE user_id = ?`, userID,
	).Scan(&np.ID, &np.UserID, &np.IssueOpen, &np.Reminders,
		&np.Published, &np.CommentNotify, &np.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting notification preferences: %w", err)
	}

	return &np, nil
}

// UpsertNotificationPreferences creates or updates notification preferences.
func (s *Store) UpsertNotificationPreferences(
	ctx context.Context, prefs *NotificationPreferences,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO notification_preferences
		 (user_id, issue_open, reminders, published,
		  comment_notify, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(user_id) DO UPDATE SET
			issue_open = excluded.issue_open,
			reminders = excluded.reminders,
			published = excluded.published,
			comment_notify = excluded.comment_notify,
			updated_at = CURRENT_TIMESTAMP`,
		prefs.UserID, prefs.IssueOpen, prefs.Reminders,
		prefs.Published, prefs.CommentNotify,
	)
	if err != nil {
		return fmt.Errorf("upserting notification preferences: %w", err)
	}

	return nil
}

// EnsureNotificationPreferences creates a default preferences row if missing.
func (s *Store) EnsureNotificationPreferences(
	ctx context.Context, userID int64,
) error {
	_, err := s.write.ExecContext(ctx,
		"INSERT OR IGNORE INTO notification_preferences (user_id) VALUES (?)",
		userID,
	)
	if err != nil {
		return fmt.Errorf("ensuring notification preferences: %w", err)
	}

	return nil
}
