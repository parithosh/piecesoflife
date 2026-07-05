// Package scheduler runs a background goroutine that dispatches timed events:
// issue reminders, auto-publish, token cleanup, and session cleanup. Events
// are persisted in the scheduler_events table so overdue ones fire on the
// next startup (catch-up recovery).
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// Actions is the interface the scheduler uses to perform business logic that
// lives in the server package. Defined here so the scheduler has no import
// cycle with server; the server satisfies it via its exported methods.
type Actions interface {
	SendReminderForIssue(
		ctx context.Context, issueID int64, isFinal bool, schedulerEventID *int64,
	) error
	SendAdminSummaryForIssue(
		ctx context.Context, issueID int64, schedulerEventID *int64,
	) error
	AutoPublishIssue(ctx context.Context, issueID int64) error
	CreateNextIssue(ctx context.Context, groupID int64) error
}

// Scheduler dispatches timed scheduler_events.
type Scheduler struct {
	store   *store.Store
	actions Actions
	logger  *slog.Logger

	tickInterval time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New constructs a Scheduler. Start must be called to begin dispatching.
func New(st *store.Store, actions Actions, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:        st,
		actions:      actions,
		logger:       logger.With(slog.String("component", "scheduler")),
		tickInterval: 60 * time.Second,
	}
}

// Start launches the scheduler goroutine. Safe to call exactly once. On
// startup it fires any overdue events (catch-up), ensures the daily cleanup
// events are scheduled, then enters a 60s tick loop.
func (s *Scheduler) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.logger.InfoContext(ctx, "Scheduler starting")

		// 1. Recovery: fire any events that were scheduled while we were down.
		s.fireOverdueEvents(ctx, true)

		// 2. Make sure the daily cleanup events are queued. If the app has
		//    been running for a while without these, this is a no-op after
		//    the first run thanks to the UNIQUE(issue_id, event_type,
		//    scheduled_at) constraint.
		s.scheduleDailyCleanup(ctx)

		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				s.logger.InfoContext(ctx, "Scheduler stopping")
				return
			case <-ticker.C:
				s.fireOverdueEvents(ctx, false)
			}
		}
	}()
}

// Stop cancels the scheduler and waits for the goroutine to exit.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) fireOverdueEvents(ctx context.Context, startup bool) {
	events, err := s.store.GetOverdueEvents(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to query overdue events",
			slog.String("error", err.Error()))
		return
	}

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return
		}

		wasLate := startup && time.Since(ev.ScheduledAt) > 5*time.Minute

		if wasLate {
			s.logger.InfoContext(ctx, "Firing late event",
				slog.String("event_type", ev.EventType),
				slog.Time("scheduled_at", ev.ScheduledAt),
				slog.Duration("delay", time.Since(ev.ScheduledAt)),
			)
		}

		s.fireEvent(ctx, ev, wasLate)
	}

	// Reschedule the daily cleanup trio after a tick so the next day's
	// events exist. Cheap — UNIQUE prevents duplicates.
	if !startup {
		s.scheduleDailyCleanup(ctx)
	}
}

func (s *Scheduler) fireEvent(
	ctx context.Context, ev store.SchedulerEvent, wasLate bool,
) {
	logger := s.logger.With(
		slog.Int64("event_id", ev.ID),
		slog.String("event_type", ev.EventType),
	)

	var err error

	switch ev.EventType {
	case "reminder_1":
		if ev.IssueID == nil {
			err = errInvalidEvent("reminder_1 missing issue_id")
			break
		}
		err = s.actions.SendReminderForIssue(ctx, *ev.IssueID, false, &ev.ID)

	case "reminder_2":
		if ev.IssueID == nil {
			err = errInvalidEvent("reminder_2 missing issue_id")
			break
		}
		err = s.actions.SendReminderForIssue(ctx, *ev.IssueID, true, &ev.ID)

	case "admin_summary":
		if ev.IssueID == nil {
			err = errInvalidEvent("admin_summary missing issue_id")
			break
		}
		err = s.actions.SendAdminSummaryForIssue(ctx, *ev.IssueID, &ev.ID)

	case "auto_close":
		if ev.IssueID == nil {
			err = errInvalidEvent("auto_close missing issue_id")
			break
		}
		err = s.actions.AutoPublishIssue(ctx, *ev.IssueID)

	case "create_next_issue":
		// The event references the pre-created draft it should open, which
		// is also how it knows its Loop. Events queued before multi-group
		// (issue_id NULL) fall back to the instance's oldest Loop.
		var groupID int64

		if ev.IssueID != nil {
			issue, issueErr := s.store.GetIssueByID(ctx, *ev.IssueID)
			if issueErr != nil {
				err = issueErr
				break
			}

			groupID = issue.GroupID
		} else {
			groupID, err = s.store.GetOldestActiveGroupID(ctx)
			if err != nil {
				break
			}
		}

		err = s.actions.CreateNextIssue(ctx, groupID)

	case "token_cleanup":
		var n int64
		n, err = s.store.CleanupExpiredTokens(ctx)
		if err == nil {
			logger.InfoContext(ctx, "Token cleanup complete",
				slog.Int64("deleted", n))
		}

	case "session_cleanup":
		var n int64
		n, err = s.store.CleanupExpiredSessions(ctx)
		if err == nil {
			logger.InfoContext(ctx, "Session cleanup complete",
				slog.Int64("deleted", n))
		}

	default:
		err = errInvalidEvent("unknown event type: " + ev.EventType)
	}

	if err != nil {
		logger.ErrorContext(ctx, "Event handler failed",
			slog.String("error", err.Error()))
		return
	}

	if markErr := s.store.MarkEventFired(ctx, ev.ID, wasLate); markErr != nil {
		logger.ErrorContext(ctx, "Failed to mark event fired",
			slog.String("error", markErr.Error()))
	}
}

// scheduleDailyCleanup ensures token_cleanup and session_cleanup events
// exist for the next UTC midnight. The UNIQUE
// constraint on (issue_id, event_type, scheduled_at) makes this idempotent
// within a given day.
func (s *Scheduler) scheduleDailyCleanup(ctx context.Context) {
	now := time.Now().UTC()
	nextMidnight := time.Date(
		now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC,
	)

	for _, eventType := range []string{
		"token_cleanup", "session_cleanup",
	} {
		if err := s.store.CreateSchedulerEvent(ctx, nil, eventType, nextMidnight); err != nil {
			// UNIQUE violation is expected on repeat scheduling. Only log
			// if the error message doesn't look like a uniqueness error.
			s.logger.DebugContext(ctx, "Daily cleanup create returned error "+
				"(usually UNIQUE — benign)",
				slog.String("event_type", eventType),
				slog.String("error", err.Error()),
			)
		}
	}
}

type schedulerError string

func (e schedulerError) Error() string { return string(e) }

func errInvalidEvent(msg string) error {
	return schedulerError("invalid scheduler event: " + msg)
}
