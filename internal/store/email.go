package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EmailLog records a sent or attempted email.
type EmailLog struct {
	ID               int64      `json:"id"`
	GroupID          *int64     `json:"group_id"`
	UserID           *int64     `json:"user_id"`
	IssueID          *int64     `json:"issue_id"`
	SchedulerEventID *int64     `json:"scheduler_event_id,omitempty"`
	Type             string     `json:"type"`
	Status           string     `json:"status"`
	SentAt           *time.Time `json:"sent_at"`
	Error            *string    `json:"error"`
	CreatedAt        time.Time  `json:"created_at"`
	// RecipientName and RecipientEmail are resolved from users by
	// ListEmailLogs; nil on other read paths or when the row has no user.
	RecipientName  *string `json:"recipient_name,omitempty"`
	RecipientEmail *string `json:"recipient_email,omitempty"`
}

// LogEmail records an email send attempt and returns the log entry ID.
func (s *Store) LogEmail(
	ctx context.Context,
	groupID, userID, issueID *int64,
	emailType, status string,
	sendErr *string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO email_log (group_id, user_id, issue_id, type, status, error)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		groupID, userID, issueID, emailType, status, sendErr,
	)
	if err != nil {
		return 0, fmt.Errorf("logging email: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting email log id: %w", err)
	}

	return id, nil
}

// BeginSchedulerEmailAttempt reserves an email_log row for one recipient of a
// scheduler event. It returns shouldSend=false when this event/recipient/type
// already has a successful send, making scheduler retries idempotent.
func (s *Store) BeginSchedulerEmailAttempt(
	ctx context.Context,
	schedulerEventID, userID int64,
	groupID, issueID *int64,
	emailType string,
) (logID int64, shouldSend bool, err error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("beginning scheduler email transaction: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT id, status
		 FROM email_log
		 WHERE scheduler_event_id = ? AND user_id = ? AND type = ?
		 LIMIT 1`,
		schedulerEventID, userID, emailType,
	).Scan(&logID, &status)
	if err == nil {
		if status == "sent" {
			if err := tx.Commit(); err != nil {
				return 0, false, fmt.Errorf("committing sent email skip: %w", err)
			}

			return logID, false, nil
		}

		_, err = tx.ExecContext(ctx,
			`UPDATE email_log
			 SET group_id = ?, issue_id = ?,
			     status = 'pending', sent_at = NULL, error = NULL
			 WHERE id = ?`,
			groupID, issueID, logID,
		)
		if err != nil {
			return 0, false, fmt.Errorf("resetting scheduler email log: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return 0, false, fmt.Errorf("committing scheduler email retry: %w", err)
		}

		return logID, true, nil
	}

	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("checking scheduler email log: %w", err)
	}

	result, err := tx.ExecContext(ctx,
		`INSERT INTO email_log
		 (group_id, user_id, issue_id, scheduler_event_id, type, status)
		 VALUES (?, ?, ?, ?, ?, 'pending')`,
		groupID, userID, issueID, schedulerEventID, emailType,
	)
	if err != nil {
		return 0, false, fmt.Errorf("creating scheduler email log: %w", err)
	}

	logID, err = result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("getting scheduler email log id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("committing scheduler email attempt: %w", err)
	}

	return logID, true, nil
}

// UpdateEmailLog updates an email log entry's status.
func (s *Store) UpdateEmailLog(
	ctx context.Context,
	id int64, status string,
	sentAt *time.Time, sendErr *string,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE email_log SET status = ?, sent_at = ?, error = ?
		 WHERE id = ?`,
		status, sentAt, sendErr, id,
	)
	if err != nil {
		return fmt.Errorf("updating email log %d: %w", id, err)
	}

	return nil
}

// ListEmailLogs returns paginated email log entries.
func (s *Store) ListEmailLogs(
	ctx context.Context, groupID int64, page, perPage int,
) ([]EmailLog, int, error) {
	var total int

	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM email_log WHERE group_id = ?", groupID,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("counting email logs: %w", err)
	}

	offset := (page - 1) * perPage

	rows, err := s.read.QueryContext(ctx,
		`SELECT l.id, l.group_id, l.user_id, l.issue_id, l.scheduler_event_id,
		        l.type, l.status, l.sent_at, l.error, l.created_at,
		        u.name, u.email
		 FROM email_log l
		 LEFT JOIN users u ON u.id = l.user_id
		 WHERE l.group_id = ?
		 ORDER BY l.created_at DESC
		 LIMIT ? OFFSET ?`, groupID, perPage, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("listing email logs: %w", err)
	}

	defer rows.Close()

	var logs []EmailLog

	for rows.Next() {
		var l EmailLog

		err := rows.Scan(&l.ID, &l.GroupID, &l.UserID, &l.IssueID,
			&l.SchedulerEventID, &l.Type, &l.Status, &l.SentAt,
			&l.Error, &l.CreatedAt, &l.RecipientName, &l.RecipientEmail)
		if err != nil {
			return nil, 0, fmt.Errorf("scanning email log: %w", err)
		}

		logs = append(logs, l)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating email logs: %w", err)
	}

	if logs == nil {
		logs = make([]EmailLog, 0)
	}

	return logs, total, nil
}

// GetEmailLogByID returns an email log entry by ID.
func (s *Store) GetEmailLogByID(
	ctx context.Context, id int64,
) (*EmailLog, error) {
	var l EmailLog

	err := s.read.QueryRowContext(ctx,
		`SELECT id, group_id, user_id, issue_id, scheduler_event_id,
		        type, status, sent_at, error, created_at
		 FROM email_log WHERE id = ?`, id,
	).Scan(&l.ID, &l.GroupID, &l.UserID, &l.IssueID, &l.SchedulerEventID,
		&l.Type, &l.Status, &l.SentAt, &l.Error, &l.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting email log %d: %w", id, err)
	}

	return &l, nil
}
