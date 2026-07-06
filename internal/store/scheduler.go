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

// GetNextPendingLegacyEventByType returns the earliest unfired event of the
// given type that carries no issue reference. Only events queued before
// multi-group (migration 016) look like this — they can only belong to the
// instance's original Loop.
func (s *Store) GetNextPendingLegacyEventByType(
	ctx context.Context, eventType string,
) (*SchedulerEvent, error) {
	var e SchedulerEvent

	err := s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, event_type, scheduled_at,
		        fired_at, was_late, created_at
		 FROM scheduler_events
		 WHERE fired_at IS NULL AND event_type = ? AND issue_id IS NULL
		 ORDER BY scheduled_at ASC LIMIT 1`,
		eventType,
	).Scan(&e.ID, &e.IssueID, &e.EventType, &e.ScheduledAt,
		&e.FiredAt, &e.WasLate, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting next pending legacy %s event: %w", eventType, err)
	}

	return &e, nil
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

// GetNextPendingEventForGroup returns the earliest unfired event of the
// given type belonging to one group (resolved through the event's issue),
// or (nil, nil) when none is queued.
func (s *Store) GetNextPendingEventForGroup(
	ctx context.Context, eventType string, groupID int64,
) (*SchedulerEvent, error) {
	var ev SchedulerEvent

	err := s.read.QueryRowContext(ctx,
		`SELECT e.id, e.issue_id, e.event_type, e.scheduled_at,
		        e.fired_at, e.was_late, e.created_at
		 FROM scheduler_events e
		 JOIN issues i ON i.id = e.issue_id
		 WHERE e.event_type = ? AND i.group_id = ? AND e.fired_at IS NULL
		 ORDER BY e.scheduled_at ASC
		 LIMIT 1`, eventType, groupID,
	).Scan(&ev.ID, &ev.IssueID, &ev.EventType, &ev.ScheduledAt,
		&ev.FiredAt, &ev.WasLate, &ev.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("getting next %s event for group %d: %w",
			eventType, groupID, err)
	}

	return &ev, nil
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
