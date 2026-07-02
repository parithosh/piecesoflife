package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SchedulerEvent tracks scheduled jobs to prevent re-execution after restarts.
type SchedulerEvent struct {
	ID          int64      `json:"id"`
	IssueID     *int64     `json:"issue_id"`
	EventType   string     `json:"event_type"`
	ScheduledAt time.Time  `json:"scheduled_at"`
	FiredAt     *time.Time `json:"fired_at"`
	WasLate     bool       `json:"was_late"`
	CreatedAt   time.Time  `json:"created_at"`
}

// CreateSchedulerEvent records a new scheduled event.
func (s *Store) CreateSchedulerEvent(
	ctx context.Context,
	issueID *int64, eventType string, scheduledAt time.Time,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO scheduler_events
		 (issue_id, event_type, scheduled_at)
		 VALUES (?, ?, ?)`,
		issueID, eventType, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("creating scheduler event: %w", err)
	}

	return nil
}

// GetPendingEvents returns all events that haven't fired yet.
func (s *Store) GetPendingEvents(
	ctx context.Context,
) ([]SchedulerEvent, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, issue_id, event_type, scheduled_at,
		        fired_at, was_late, created_at
		 FROM scheduler_events WHERE fired_at IS NULL
		 ORDER BY scheduled_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("getting pending events: %w", err)
	}

	defer rows.Close()

	return scanSchedulerEvents(rows)
}

// GetOverdueEvents returns unfired events past their scheduled time.
func (s *Store) GetOverdueEvents(
	ctx context.Context,
) ([]SchedulerEvent, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, issue_id, event_type, scheduled_at,
		        fired_at, was_late, created_at
		 FROM scheduler_events
		 WHERE fired_at IS NULL AND scheduled_at <= CURRENT_TIMESTAMP
		 ORDER BY scheduled_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("getting overdue events: %w", err)
	}

	defer rows.Close()

	return scanSchedulerEvents(rows)
}

// GetNextPendingEvent returns the next event that needs to fire.
func (s *Store) GetNextPendingEvent(
	ctx context.Context,
) (*SchedulerEvent, error) {
	var e SchedulerEvent

	err := s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, event_type, scheduled_at,
		        fired_at, was_late, created_at
		 FROM scheduler_events
		 WHERE fired_at IS NULL
		 ORDER BY scheduled_at ASC LIMIT 1`,
	).Scan(&e.ID, &e.IssueID, &e.EventType, &e.ScheduledAt,
		&e.FiredAt, &e.WasLate, &e.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting next pending event: %w", err)
	}

	return &e, nil
}

// GetNextPendingEventByType returns the earliest unfired event of the given
// type, or (nil, nil) when none is queued. Used to surface "next issue opens
// on …" in the UI without touching scheduler state.
func (s *Store) GetNextPendingEventByType(
	ctx context.Context, eventType string,
) (*SchedulerEvent, error) {
	var e SchedulerEvent

	err := s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, event_type, scheduled_at,
		        fired_at, was_late, created_at
		 FROM scheduler_events
		 WHERE fired_at IS NULL AND event_type = ?
		 ORDER BY scheduled_at ASC LIMIT 1`,
		eventType,
	).Scan(&e.ID, &e.IssueID, &e.EventType, &e.ScheduledAt,
		&e.FiredAt, &e.WasLate, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting next pending %s event: %w", eventType, err)
	}

	return &e, nil
}

// GetLastFiredAt returns the most recent fired_at timestamp for a given event
// type. Returns (nil, nil) if no event of that type has fired yet.
func (s *Store) GetLastFiredAt(
	ctx context.Context, eventType string,
) (*time.Time, error) {
	var raw sql.NullString

	err := s.read.QueryRowContext(ctx,
		`SELECT fired_at FROM scheduler_events
		 WHERE event_type = ? AND fired_at IS NOT NULL
		 ORDER BY datetime(fired_at) DESC
		 LIMIT 1`,
		eventType,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting last fired_at for %s: %w", eventType, err)
	}

	if !raw.Valid || raw.String == "" {
		return nil, nil
	}

	v, err := parseSQLiteTime(raw.String)
	if err != nil {
		return nil, fmt.Errorf("parsing last fired_at for %s: %w", eventType, err)
	}

	return &v, nil
}

func parseSQLiteTime(raw string) (time.Time, error) {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}

	var lastErr error
	for _, format := range formats {
		t, err := time.ParseInLocation(format, raw, time.UTC)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}

	return time.Time{}, lastErr
}

// MarkEventFired records that a scheduled event has been executed.
func (s *Store) MarkEventFired(
	ctx context.Context, id int64, wasLate bool,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE scheduler_events SET fired_at = CURRENT_TIMESTAMP,
		 was_late = ? WHERE id = ?`,
		wasLate, id,
	)
	if err != nil {
		return fmt.Errorf("marking event %d as fired: %w", id, err)
	}

	return nil
}

// DeleteEventsForIssue removes all scheduled events for an issue.
func (s *Store) DeleteEventsForIssue(
	ctx context.Context, issueID int64,
) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM scheduler_events WHERE issue_id = ?", issueID,
	)
	if err != nil {
		return fmt.Errorf("deleting events for issue %d: %w", issueID, err)
	}

	return nil
}

func scanSchedulerEvents(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
},
) ([]SchedulerEvent, error) {
	var events []SchedulerEvent

	for rows.Next() {
		var e SchedulerEvent

		err := rows.Scan(&e.ID, &e.IssueID, &e.EventType,
			&e.ScheduledAt, &e.FiredAt, &e.WasLate, &e.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning scheduler event: %w", err)
		}

		events = append(events, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating scheduler events: %w", err)
	}

	if events == nil {
		events = make([]SchedulerEvent, 0)
	}

	return events, nil
}
