package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PendingCommentNotification is one queued "someone commented on your
// content" row, joined with everything the daily digest email needs. The
// digest reads bodies at send time, so edits made before the send are
// reflected and deleted comments (CASCADE) never appear.
type PendingCommentNotification struct {
	NotificationID int64     `json:"notification_id"`
	RecipientID    int64     `json:"recipient_id"`
	RecipientName  string    `json:"recipient_name"`
	RecipientEmail string    `json:"recipient_email"`
	CommentID      int64     `json:"comment_id"`
	CommenterName  string    `json:"commenter_name"`
	Body           string    `json:"body"`
	ResponseID     *int64    `json:"response_id"`
	DiaryDayID     *int64    `json:"diary_day_id"`
	DumpItemID     *int64    `json:"dump_item_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListThreadParticipants returns the distinct authors of a comment thread —
// the top-level comment plus every reply under it. Threads are one level
// deep, so this is exactly "everyone who wrote in this conversation".
func (s *Store) ListThreadParticipants(
	ctx context.Context, topLevelCommentID int64,
) ([]int64, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT DISTINCT user_id FROM comments
		 WHERE id = ? OR parent_id = ?`,
		topLevelCommentID, topLevelCommentID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing thread participants: %w", err)
	}
	defer rows.Close()

	ids := make([]int64, 0, 8)

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning thread participant: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating thread participants: %w", err)
	}

	return ids, nil
}

// EnqueueCommentNotification queues a digest row for the owner of the
// commented content. Duplicate (recipient, comment) pairs are ignored.
func (s *Store) EnqueueCommentNotification(
	ctx context.Context, recipientID, commentID int64,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT OR IGNORE INTO comment_notifications (recipient_id, comment_id)
		 VALUES (?, ?)`,
		recipientID, commentID,
	)
	if err != nil {
		return fmt.Errorf("enqueueing comment notification: %w", err)
	}

	return nil
}

// ListPendingCommentNotifications returns every queued digest row, grouped
// by recipient (then comment age) so the sender can walk recipients in one
// pass.
func (s *Store) ListPendingCommentNotifications(
	ctx context.Context,
) ([]PendingCommentNotification, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT n.id, n.recipient_id, r.name, r.email,
		        c.id, u.name, c.body,
		        c.response_id, c.diary_day_id, c.dump_item_id, c.created_at
		 FROM comment_notifications n
		 JOIN comments c ON c.id = n.comment_id
		 JOIN users u ON u.id = c.user_id
		 JOIN users r ON r.id = n.recipient_id
		 ORDER BY n.recipient_id, c.created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing pending comment notifications: %w", err)
	}
	defer rows.Close()

	pending := make([]PendingCommentNotification, 0, 16)

	for rows.Next() {
		var p PendingCommentNotification

		err := rows.Scan(&p.NotificationID, &p.RecipientID, &p.RecipientName,
			&p.RecipientEmail, &p.CommentID, &p.CommenterName, &p.Body,
			&p.ResponseID, &p.DiaryDayID, &p.DumpItemID, &p.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning pending notification: %w", err)
		}

		pending = append(pending, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pending notifications: %w", err)
	}

	return pending, nil
}

// DeleteCommentNotifications drains queued rows by ID — called after a
// recipient's digest is handed to the mailer (or when their preference says
// never to send one).
func (s *Store) DeleteCommentNotifications(
	ctx context.Context, ids []int64,
) error {
	if len(ids) == 0 {
		return nil
	}

	placeholders := strings.TrimSuffix(
		strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))

	for _, id := range ids {
		args = append(args, id)
	}

	_, err := s.write.ExecContext(ctx,
		"DELETE FROM comment_notifications WHERE id IN ("+placeholders+")",
		args...,
	)
	if err != nil {
		return fmt.Errorf("deleting comment notifications: %w", err)
	}

	return nil
}
