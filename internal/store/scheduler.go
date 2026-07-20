package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrScheduleOverlap means a collecting round and its queued successor
// would overlap. ApplySchedule checks this inside its write transaction so
// concurrent legacy and dashboard requests cannot race past HTTP validation.
var ErrScheduleOverlap = errors.New("current and next round schedules overlap")

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

// SchedulerEventSpec describes one pending event to insert as part of an
// atomic schedule update.
type SchedulerEventSpec struct {
	EventType   string
	ScheduledAt time.Time
}

// ScheduleUpdate is the database-side half of the admin schedule editor.
// Current* replaces a collecting round's deadline and pending lifecycle
// events. Next* persists the answering window, moves or creates the next
// draft, and replaces its queued open event. When both halves are present,
// ApplySchedule commits them together.
type ScheduleUpdate struct {
	GroupID int64

	CurrentIssueID  *int64
	CurrentDeadline *time.Time
	// CurrentTitle optionally renames the round in the same transaction —
	// a combined title+deadline update must not half-apply, leaving the
	// deadline (and every member's reminder emails) moved behind an error
	// response. Nil leaves the title untouched.
	CurrentTitle  *string
	CurrentEvents []SchedulerEventSpec

	NextOpen   *time.Time
	NextClose  *time.Time
	NextMonth  int
	NextYear   int
	WindowDays int
}

// ApplySchedule atomically applies a live-round reschedule and/or a pinned
// next round. In particular, the old scheduler events are never deleted
// unless every replacement row and issue/settings update can also commit.
func (s *Store) ApplySchedule(
	ctx context.Context, update ScheduleUpdate,
) (*int64, error) {
	if (update.CurrentIssueID == nil) != (update.CurrentDeadline == nil) {
		return nil, fmt.Errorf("current issue and deadline must be provided together")
	}
	if (update.NextOpen == nil) != (update.NextClose == nil) {
		return nil, fmt.Errorf("next open and close must be provided together")
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning schedule update: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	switch {
	case update.CurrentDeadline != nil && update.NextOpen != nil:
		if !update.NextOpen.After(*update.CurrentDeadline) {
			return nil, fmt.Errorf("next round opens before current deadline: %w",
				ErrScheduleOverlap)
		}
	case update.CurrentDeadline != nil:
		var pinnedOpen time.Time
		pinErr := tx.QueryRowContext(ctx,
			`SELECT e.scheduled_at
			 FROM scheduler_events e
			 JOIN issues i ON i.id = e.issue_id
			 WHERE e.event_type = 'create_next_issue'
			   AND e.fired_at IS NULL AND i.group_id = ? AND i.status = 'draft'
			 ORDER BY e.scheduled_at ASC LIMIT 1`, update.GroupID,
		).Scan(&pinnedOpen)
		if pinErr != nil && !errors.Is(pinErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("checking queued next round: %w", pinErr)
		}
		if pinErr == nil && !pinnedOpen.After(*update.CurrentDeadline) {
			return nil, fmt.Errorf("current deadline reaches queued next round: %w",
				ErrScheduleOverlap)
		}
	case update.NextOpen != nil:
		var currentDeadline time.Time
		currentErr := tx.QueryRowContext(ctx,
			`SELECT deadline FROM issues
			 WHERE group_id = ? AND status = 'collecting' LIMIT 1`, update.GroupID,
		).Scan(&currentDeadline)
		if currentErr != nil && !errors.Is(currentErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("checking collecting round: %w", currentErr)
		}
		if currentErr == nil && !update.NextOpen.After(currentDeadline) {
			return nil, fmt.Errorf("next round opens before current deadline: %w",
				ErrScheduleOverlap)
		}
	}

	if update.CurrentIssueID != nil {
		result, execErr := tx.ExecContext(ctx,
			`UPDATE issues SET deadline = ?,
			   title = CASE WHEN ? THEN ? ELSE title END
			 WHERE id = ? AND group_id = ? AND status = 'collecting'`,
			*update.CurrentDeadline, update.CurrentTitle != nil, update.CurrentTitle,
			*update.CurrentIssueID, update.GroupID,
		)
		if execErr != nil {
			return nil, fmt.Errorf("updating issue %d deadline: %w",
				*update.CurrentIssueID, execErr)
		}

		if n, rowsErr := result.RowsAffected(); rowsErr != nil {
			return nil, fmt.Errorf("checking issue %d deadline update: %w",
				*update.CurrentIssueID, rowsErr)
		} else if n != 1 {
			return nil, fmt.Errorf("updating issue %d deadline: %w",
				*update.CurrentIssueID, sql.ErrNoRows)
		}

		if _, execErr = tx.ExecContext(ctx,
			`DELETE FROM scheduler_events
			 WHERE issue_id = ? AND fired_at IS NULL`, *update.CurrentIssueID,
		); execErr != nil {
			return nil, fmt.Errorf("clearing issue %d events: %w",
				*update.CurrentIssueID, execErr)
		}

		for _, ev := range update.CurrentEvents {
			if _, execErr = tx.ExecContext(ctx,
				`INSERT INTO scheduler_events (issue_id, event_type, scheduled_at)
				 VALUES (?, ?, ?)`,
				*update.CurrentIssueID, ev.EventType, ev.ScheduledAt,
			); execErr != nil {
				return nil, fmt.Errorf("queueing %s for issue %d: %w",
					ev.EventType, *update.CurrentIssueID, execErr)
			}
		}
	}

	var nextDraftID *int64

	if update.NextOpen != nil {
		result, execErr := tx.ExecContext(ctx,
			`UPDATE settings SET submission_window_days = ?,
			 updated_at = CURRENT_TIMESTAMP WHERE group_id = ?`,
			update.WindowDays, update.GroupID,
		)
		if execErr != nil {
			return nil, fmt.Errorf("saving submission window: %w", execErr)
		}

		if n, rowsErr := result.RowsAffected(); rowsErr != nil {
			return nil, fmt.Errorf("checking submission window update: %w", rowsErr)
		} else if n != 1 {
			return nil, fmt.Errorf("saving submission window: %w", sql.ErrNoRows)
		}

		if _, execErr = tx.ExecContext(ctx,
			`DELETE FROM scheduler_events
			 WHERE fired_at IS NULL AND event_type = 'create_next_issue'
			   AND issue_id IN (SELECT id FROM issues WHERE group_id = ?)`,
			update.GroupID,
		); execErr != nil {
			return nil, fmt.Errorf("clearing queued next round: %w", execErr)
		}

		var draftID int64
		draftErr := tx.QueryRowContext(ctx,
			`SELECT id FROM issues
			 WHERE group_id = ? AND status = 'draft'
			 ORDER BY opens_at ASC LIMIT 1`, update.GroupID,
		).Scan(&draftID)
		if draftErr != nil && !errors.Is(draftErr, sql.ErrNoRows) {
			return nil, fmt.Errorf("checking for draft round: %w", draftErr)
		}

		excludeID := int64(0)
		if draftErr == nil {
			excludeID = draftID
		}

		month, year, labelErr := nextAvailableIssueLabelTx(
			ctx, tx, update.GroupID, excludeID, update.NextMonth, update.NextYear,
		)
		if labelErr != nil {
			return nil, labelErr
		}

		if draftErr == nil {
			result, execErr = tx.ExecContext(ctx,
				`UPDATE issues SET month = ?, year = ?, opens_at = ?, deadline = ?
				 WHERE id = ? AND group_id = ? AND status = 'draft'`,
				month, year, *update.NextOpen, *update.NextClose,
				draftID, update.GroupID,
			)
			if execErr != nil {
				return nil, fmt.Errorf("updating draft %d schedule: %w", draftID, execErr)
			}

			if n, rowsErr := result.RowsAffected(); rowsErr != nil {
				return nil, fmt.Errorf("checking draft %d update: %w", draftID, rowsErr)
			} else if n != 1 {
				return nil, fmt.Errorf("updating draft %d schedule: %w", draftID, sql.ErrNoRows)
			}
		} else {
			result, execErr = tx.ExecContext(ctx,
				`INSERT INTO issues
				 (group_id, title, month, year, opens_at, deadline, count_admin_in)
				 VALUES (?, NULL, ?, ?, ?, ?, 1)`,
				update.GroupID, month, year, *update.NextOpen, *update.NextClose,
			)
			if execErr != nil {
				return nil, fmt.Errorf("pre-creating next round: %w", execErr)
			}

			draftID, execErr = result.LastInsertId()
			if execErr != nil {
				return nil, fmt.Errorf("getting next round id: %w", execErr)
			}
		}

		if _, execErr = tx.ExecContext(ctx,
			`INSERT INTO scheduler_events (issue_id, event_type, scheduled_at)
			 VALUES (?, 'create_next_issue', ?)`, draftID, *update.NextOpen,
		); execErr != nil {
			return nil, fmt.Errorf("queueing next round open: %w", execErr)
		}

		nextDraftID = &draftID
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing schedule update: %w", err)
	}

	return nextDraftID, nil
}

// NextAvailableIssueLabel picks the first unused archive (month, year) label
// at or after the requested month — the archive addresses issues as
// /issues/{year}/{month}, so a duplicate label would leave one edition
// permanently unreachable. excludeIssueID is the issue being relabeled
// (0 when creating), whose own label doesn't count as taken.
func (s *Store) NextAvailableIssueLabel(
	ctx context.Context, groupID, excludeIssueID int64,
	baseMonth, baseYear int,
) (int, int, error) {
	return nextAvailableIssueLabel(ctx, s.read, groupID, excludeIssueID, baseMonth, baseYear)
}

// nextAvailableIssueLabelTx is NextAvailableIssueLabel inside the schedule
// write transaction, where it prevents two concurrent saves from choosing
// the same label.
func nextAvailableIssueLabelTx(
	ctx context.Context, tx *sql.Tx, groupID, excludeIssueID int64,
	baseMonth, baseYear int,
) (int, int, error) {
	return nextAvailableIssueLabel(ctx, tx, groupID, excludeIssueID, baseMonth, baseYear)
}

// rowQuerier is the subset of *sql.DB / *sql.Tx the label walk needs, so
// transactional and plain paths share one implementation instead of two
// drifting copies.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func nextAvailableIssueLabel(
	ctx context.Context, q rowQuerier, groupID, excludeIssueID int64,
	baseMonth, baseYear int,
) (int, int, error) {
	for offset := 0; offset < 1200; offset++ {
		monthIndex := baseMonth - 1 + offset
		month := monthIndex%12 + 1
		year := baseYear + monthIndex/12

		var occupied int
		err := q.QueryRowContext(ctx,
			`SELECT 1 FROM issues
			 WHERE group_id = ? AND month = ? AND year = ? AND id != ?
			 LIMIT 1`,
			groupID, month, year, excludeIssueID,
		).Scan(&occupied)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return month, year, nil
		case err != nil:
			return 0, 0, fmt.Errorf("checking issue label %d-%02d: %w", year, month, err)
		}
	}

	return 0, 0, fmt.Errorf("no free issue label found after %d-%02d", baseYear, baseMonth)
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

// EnsureDailyEvent queues an issue-less daily event (cleanups, comment
// digest) unless one already exists for that moment. Daily events can't
// lean on UNIQUE(issue_id, event_type, scheduled_at) — SQLite treats their
// NULL issue_id as distinct — so dedupe comes from the partial unique index
// ux_scheduler_events_daily (migration 020) plus INSERT OR IGNORE.
func (s *Store) EnsureDailyEvent(
	ctx context.Context, eventType string, scheduledAt time.Time,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT OR IGNORE INTO scheduler_events
		 (issue_id, event_type, scheduled_at)
		 VALUES (NULL, ?, ?)`,
		eventType, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("ensuring daily event %s: %w", eventType, err)
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

// DeletePendingEventsForGroup removes every unfired event belonging to a
// group's issues — called when a Loop is archived so its rounds stop
// closing, publishing, and emailing. Fired events stay as history.
func (s *Store) DeletePendingEventsForGroup(
	ctx context.Context, groupID int64,
) (int64, error) {
	return s.DeletePendingGroupEventsByType(ctx, groupID, "")
}

// RequeueIssueEvents resets an issue's scheduled lifecycle in one
// transaction: every unfired event is dropped and each spec queued anew.
// Fired rows can't simply be deleted — they are history and email_log
// references them — yet UNIQUE(issue_id, event_type, scheduled_at) bars an
// identical pending twin next to one. So a spec that collides with an
// already-fired row (the launch wizard re-running a round whose earlier
// attempt already auto-closed) re-arms that row in place instead.
func (s *Store) RequeueIssueEvents(
	ctx context.Context, issueID int64, events []SchedulerEventSpec,
) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning event requeue for issue %d: %w", issueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM scheduler_events
		 WHERE issue_id = ? AND fired_at IS NULL`, issueID,
	); err != nil {
		return fmt.Errorf("clearing events for issue %d: %w", issueID, err)
	}

	for _, ev := range events {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO scheduler_events (issue_id, event_type, scheduled_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(issue_id, event_type, scheduled_at)
			 DO UPDATE SET fired_at = NULL, was_late = 0`,
			issueID, ev.EventType, ev.ScheduledAt,
		); err != nil {
			return fmt.Errorf("requeueing %s for issue %d: %w",
				ev.EventType, issueID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing event requeue for issue %d: %w", issueID, err)
	}

	return nil
}

// GetPendingNextRoundEvent returns the earliest unfired create_next_issue
// event whose round is still a draft — the genuinely queued next round.
// Stale events pointing at rounds that already opened (started manually
// before the event fired, or legacy data) are skipped. (nil, nil) when
// nothing genuine is queued.
func (s *Store) GetPendingNextRoundEvent(
	ctx context.Context, groupID int64,
) (*SchedulerEvent, error) {
	var ev SchedulerEvent

	err := s.read.QueryRowContext(ctx,
		`SELECT e.id, e.issue_id, e.event_type, e.scheduled_at,
		        e.fired_at, e.was_late, e.created_at
		 FROM scheduler_events e
		 JOIN issues i ON i.id = e.issue_id
		 WHERE e.event_type = 'create_next_issue'
		   AND i.group_id = ? AND i.status = 'draft'
		   AND e.fired_at IS NULL
		 ORDER BY e.scheduled_at ASC
		 LIMIT 1`, groupID,
	).Scan(&ev.ID, &ev.IssueID, &ev.EventType, &ev.ScheduledAt,
		&ev.FiredAt, &ev.WasLate, &ev.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("getting pending next round event for group %d: %w",
			groupID, err)
	}

	return &ev, nil
}

// DeletePendingGroupEventsByType removes every unfired event belonging to a
// group's issues, narrowed to one event type when eventType is non-empty
// ("" removes all types). Used when an admin pins or manually opens the
// next round, so stale create_next_issue events can't fire later and open
// a round off its date — and by Loop archiving via
// DeletePendingEventsForGroup. Fired events stay as history. Returns the
// number of events removed.
func (s *Store) DeletePendingGroupEventsByType(
	ctx context.Context, groupID int64, eventType string,
) (int64, error) {
	query := `DELETE FROM scheduler_events
		 WHERE fired_at IS NULL
		   AND issue_id IN (SELECT id FROM issues WHERE group_id = ?)`
	args := make([]any, 0, 2)
	args = append(args, groupID)

	if eventType != "" {
		query += ` AND event_type = ?`

		args = append(args, eventType)
	}

	result, err := s.write.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("deleting pending %q events for group %d: %w",
			eventType, groupID, err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted events for group %d: %w", groupID, err)
	}

	return n, nil
}

// HasRecentFiredReminder reports whether any reminder event for the issue
// fired within the last 24 hours. Guards the immediate last-chance reminder
// queued when a deadline move lands too close for the standard slots —
// successive schedule saves must not send non-responders duplicate
// "last chance" emails.
func (s *Store) HasRecentFiredReminder(
	ctx context.Context, issueID int64,
) (bool, error) {
	var exists bool

	err := s.read.QueryRowContext(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM scheduler_events
			WHERE issue_id = ?
			  AND event_type IN ('reminder_1', 'reminder_2')
			  AND fired_at >= datetime('now', '-1 day')
		)`, issueID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking recent reminders for issue %d: %w", issueID, err)
	}

	return exists, nil
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
