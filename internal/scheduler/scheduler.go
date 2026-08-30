// Package scheduler runs a background goroutine that dispatches timed events:
// issue reminders, auto-publish, token cleanup, and session cleanup. Events
// are persisted in the scheduler_events table so overdue ones fire on the
// next startup (catch-up recovery).
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
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
	CreateNextIssue(ctx context.Context, groupID int64, scheduledAt time.Time) error
	ReconcileAutoCreate(ctx context.Context) error
	SendCommentDigests(ctx context.Context) error
	CheckUploadIntegrity(ctx context.Context) error
}

// Scheduler dispatches timed scheduler_events.
type Scheduler struct {
	store   *store.Store
	actions Actions
	logger  *slog.Logger

	tickInterval time.Duration

	// eventTimeout bounds a single event's execution. One stuck event
	// (a hung store call or email send) must never wedge the loop and
	// with it every pending event — the 2026-08-10 incident. On expiry
	// the event logs an ERROR, stays unfired, and retries next tick.
	eventTimeout time.Duration

	// staleEventCeiling is how late a member-visible event may fire.
	// Beyond it the event is skipped, not fired.
	//
	// This exists because of the 2026-08-05 incident: a restore handed the
	// app a month-old database, and the catch-up pass fired reminder_1
	// (23 days late), reminder_2 (18 days) and auto_close (16 days) within
	// 16 seconds — mailing members to answer a round they had already
	// finished, then publishing it. Catch-up is right for a short outage
	// and wrong for a stale database, and lateness is what separates them.
	staleEventCeiling time.Duration

	// checkpointInterval is how often the loop truncates the WAL. SQLite's
	// passive autocheckpoint yields to open readers and can leave the -wal
	// growing without bound.
	checkpointInterval time.Duration

	// lastTick is the UnixNano of the scheduler's last progress — a
	// completed dispatch pass or an individual event within one — exposed
	// via LastTick so /health can detect a wedged loop without flagging a
	// long-but-moving catch-up backlog.
	lastTick atomic.Int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New constructs a Scheduler. Start must be called to begin dispatching.
func New(st *store.Store, actions Actions, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:              st,
		actions:            actions,
		logger:             logger.With(slog.String("component", "scheduler")),
		tickInterval:       60 * time.Second,
		eventTimeout:       2 * time.Minute,
		staleEventCeiling:  48 * time.Hour,
		checkpointInterval: 6 * time.Hour,
	}
}

// LastTick returns when the scheduler last completed a dispatch pass.
// Valid from Start onward; the zero time before that.
func (s *Scheduler) LastTick() time.Time {
	n := s.lastTick.Load()
	if n == 0 {
		return time.Time{}
	}

	return time.Unix(0, n)
}

// Start launches the scheduler goroutine. Safe to call exactly once. On
// startup it fires any overdue events (catch-up), ensures the daily cleanup
// events are scheduled, then enters a 60s tick loop.
func (s *Scheduler) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel

	// Seed the heartbeat before the goroutine runs so /health never sees
	// a zero LastTick between Start and the first dispatch pass.
	s.lastTick.Store(time.Now().UnixNano())

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		s.logger.InfoContext(ctx, "Scheduler starting")

		// 1. Recovery: fire any events that were scheduled while we were down.
		s.fireOverdueEvents(ctx, true)

		// 1b. Re-queue any auto-create cycle that stalled (e.g. a transient
		//     store error at publish time meant no next-round event was
		//     queued). Repeated daily from the tick loop.
		s.reconcileAutoCreate(ctx)

		lastReconcile := time.Now()
		lastCheckpoint := time.Now()

		// 2. Make sure the daily cleanup events are queued. Idempotent —
		//    EnsureDailyEvent inserts nothing when the event already exists.
		s.scheduleDailyCleanup(ctx)

		s.lastTick.Store(time.Now().UnixNano())

		// 3. Reconcile media on disk against the rows that reference it.
		//    Orphaned files mean rows disappeared — the signal that went
		//    unnoticed for three weeks after the 2026-08-05 restore.
		//
		//    Deliberately last in startup: it walks the whole uploads tree
		//    with synchronous filesystem calls, which on network storage
		//    can outlast its timeout. Nothing scheduled may queue behind a
		//    diagnostic.
		s.checkUploadIntegrity(ctx)

		s.lastTick.Store(time.Now().UnixNano())

		ticker := time.NewTicker(s.tickInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				s.logger.InfoContext(ctx, "Scheduler stopping")
				return
			case <-ticker.C:
				s.fireOverdueEvents(ctx, false)
				s.lastTick.Store(time.Now().UnixNano())

				if time.Since(lastReconcile) >= 24*time.Hour {
					lastReconcile = time.Now()
					s.reconcileAutoCreate(ctx)
					s.checkUploadIntegrity(ctx)
				}

				if time.Since(lastCheckpoint) >= s.checkpointInterval {
					lastCheckpoint = time.Now()
					s.checkpointWAL(ctx)
				}
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
	// Bounded like events — every store call the loop depends on must
	// surface a wedge as an ERROR instead of stopping the loop.
	queryCtx, queryCancel := context.WithTimeout(ctx, s.eventTimeout)
	events, err := s.store.GetOverdueEvents(queryCtx)

	queryCancel()

	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to query overdue events",
			slog.String("error", err.Error()))
		return
	}

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return
		}

		// Lateness is a property of the event, not of how we found it. It
		// used to be scoped to the startup pass, which meant an event that
		// went stale while the app was running recorded was_late = false —
		// inconsistent now that the skip gate judges the same delay on
		// every pass.
		delay := time.Since(ev.ScheduledAt)
		wasLate := delay > 5*time.Minute

		if wasLate {
			s.logger.InfoContext(ctx, "Firing late event",
				slog.String("event_type", ev.EventType),
				slog.Time("scheduled_at", ev.ScheduledAt),
				slog.Duration("delay", delay),
				slog.Bool("startup", startup),
			)
		}

		s.fireEvent(ctx, ev, wasLate)

		// Heartbeat per event, not just per pass: a catch-up backlog
		// where each event may use up to eventTimeout must read as
		// progress in /health, not as a stale scheduler.
		s.lastTick.Store(time.Now().UnixNano())
	}

	// Reschedule the daily cleanup trio after a tick so the next day's
	// events exist. Cheap — EnsureDailyEvent is a deduped no-op once the
	// events are queued.
	if !startup {
		s.scheduleDailyCleanup(ctx)
	}
}

func (s *Scheduler) fireEvent(
	parent context.Context, ev store.SchedulerEvent, wasLate bool,
) {
	// Bound the whole event. Store pool acquisition respects context
	// deadlines, so a wedged write surfaces here as an ERROR after
	// eventTimeout instead of hanging the loop.
	ctx, cancel := context.WithTimeout(parent, s.eventTimeout)
	defer cancel()

	logger := s.logger.With(
		slog.Int64("event_id", ev.ID),
		slog.String("event_type", ev.EventType),
	)

	// An archived Loop must not keep closing rounds and emailing members.
	// Archiving deletes its pending events, but events created through any
	// other path still land here — mark them fired so they never retry.
	if ev.IssueID != nil {
		issue, err := s.store.GetIssueByID(ctx, *ev.IssueID)
		if err == nil {
			if group, gErr := s.store.GetGroup(ctx, issue.GroupID); gErr == nil &&
				!group.IsActive {
				logger.InfoContext(ctx, "Skipping event for archived Loop",
					slog.Int64("group_id", issue.GroupID))

				if markErr := s.store.MarkEventFired(ctx, ev.ID, wasLate); markErr != nil {
					logger.ErrorContext(ctx, "Failed to mark skipped event fired",
						slog.String("error", markErr.Error()))
				}

				return
			}
		}
	}

	// Two guards stand between a stale event and a member's inbox.
	//
	// The semantic one is primary: a reminder for a round that is no longer
	// collecting, or an auto_close for a round already published, is wrong
	// regardless of how late it is. It also covers small delays that a
	// lateness ceiling would wave through — a container restart, clock
	// skew, or an admin editing a deadline backwards.
	//
	// The lateness ceiling is the backstop for anything the semantic check
	// cannot revalidate.
	if skip, reason := s.shouldSkipEvent(ctx, ev); skip {
		logger.ErrorContext(ctx, "Skipping stale scheduler event",
			slog.String("reason", reason),
			slog.Time("scheduled_at", ev.ScheduledAt),
			slog.Duration("delay", time.Since(ev.ScheduledAt)),
		)

		// Marked fired so it never retries. It is not coming back: the
		// round it belonged to has moved on.
		if markErr := s.store.MarkEventFired(ctx, ev.ID, true); markErr != nil {
			logger.ErrorContext(ctx, "Failed to mark skipped event fired",
				slog.String("error", markErr.Error()))
		}

		return
	}

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
		// is also how it knows its Loop (migration 017 backfilled the
		// pre-multi-group events that carried no reference).
		if ev.IssueID == nil {
			err = errInvalidEvent("create_next_issue missing issue_id")
			break
		}

		issue, issueErr := s.store.GetIssueByID(ctx, *ev.IssueID)
		if issueErr != nil {
			err = issueErr
			break
		}

		err = s.actions.CreateNextIssue(ctx, issue.GroupID, ev.ScheduledAt)

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

	case "comment_digest":
		err = s.actions.SendCommentDigests(ctx)
		if err == nil {
			logger.InfoContext(ctx, "Comment digest complete")
		}

	default:
		err = errInvalidEvent("unknown event type: " + ev.EventType)
	}

	if err != nil {
		logger.ErrorContext(ctx, "Event handler failed",
			slog.String("error", err.Error()))
		return
	}

	// Marking fired gets a fresh allowance from the parent — a slow but
	// successful handler may have consumed the whole event budget, and it
	// must not be re-fired for want of deadline headroom.
	markCtx, markCancel := context.WithTimeout(parent, 15*time.Second)
	defer markCancel()

	if markErr := s.store.MarkEventFired(markCtx, ev.ID, wasLate); markErr != nil {
		logger.ErrorContext(ctx, "Failed to mark event fired",
			slog.String("error", markErr.Error()))
	}
}

// scheduleDailyCleanup ensures the daily events — token/session cleanup and
// the comment digest — exist for the next UTC midnight. Idempotent:
// EnsureDailyEvent dedupes via the partial unique index on issue-less
// events, so repeat calls insert nothing and an error here is a real error.
func (s *Scheduler) scheduleDailyCleanup(ctx context.Context) {
	// Bounded like events: EnsureDailyEvent is a write-pool call and a
	// wedge here must not stop the loop.
	ctx, cancel := context.WithTimeout(ctx, s.eventTimeout)
	defer cancel()

	now := time.Now().UTC()
	nextMidnight := time.Date(
		now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC,
	)

	for _, eventType := range []string{
		"token_cleanup", "session_cleanup", "comment_digest",
	} {
		if err := s.store.EnsureDailyEvent(ctx, eventType, nextMidnight); err != nil {
			s.logger.ErrorContext(ctx, "Failed to schedule daily event",
				slog.String("event_type", eventType),
				slog.String("error", err.Error()),
			)
		}
	}
}

// reconcileAutoCreate runs one auto-create reconcile pass under the event
// timeout — like events, a wedged store call here must surface as an
// ERROR, not stop the loop.
func (s *Scheduler) reconcileAutoCreate(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.eventTimeout)
	defer cancel()

	if err := s.actions.ReconcileAutoCreate(ctx); err != nil {
		s.logger.ErrorContext(ctx, "Auto-create reconcile failed",
			slog.String("error", err.Error()))
	}
}

// memberVisibleEvents are the event types that email members or change what
// they see. Only these are subject to the staleness ceiling; the cleanup
// and digest events are idempotent maintenance and always safe to catch up.
var memberVisibleEvents = map[string]struct{}{
	"reminder_1":        {},
	"reminder_2":        {},
	"admin_summary":     {},
	"auto_close":        {},
	"create_next_issue": {},
}

// shouldSkipEvent decides whether an overdue event has been overtaken by
// reality. It returns the reason so the skip is greppable in logs.
//
// A failed issue lookup returns false and skips the ceiling too. Skipping
// consumes an event permanently, so a transient read error must never be
// grounds for it: better to let the handler run, fail, and retry next tick.
func (s *Scheduler) shouldSkipEvent(
	ctx context.Context, ev store.SchedulerEvent,
) (bool, string) {
	if _, visible := memberVisibleEvents[ev.EventType]; !visible {
		return false, ""
	}

	// Semantic check: is the round this event refers to still in the state
	// the event assumes?
	if ev.IssueID != nil {
		issue, err := s.store.GetIssueByID(ctx, *ev.IssueID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Cannot revalidate event, leaving it pending",
				slog.Int64("event_id", ev.ID),
				slog.String("event_type", ev.EventType),
				slog.String("error", err.Error()),
			)

			return false, ""
		}

		switch ev.EventType {
		case "reminder_1", "reminder_2", "admin_summary", "auto_close":
			// All four only make sense while the round is open.
			if issue.Status != "collecting" {
				return true, "issue is " + issue.Status + ", not collecting"
			}

		case "create_next_issue":
			// Opens a pre-created draft; anything else already opened.
			if issue.Status != "draft" {
				return true, "next issue is " + issue.Status + ", not draft"
			}
		}

		// A reminder is pointless once the deadline has passed — the round
		// is about to close, or should already have. Reminders are queued
		// at least minReminderLead (12h) before the deadline, so this can
		// only trigger on a genuine replay, never on a normal restart.
		if ev.EventType == "reminder_1" || ev.EventType == "reminder_2" {
			if time.Now().After(issue.Deadline) {
				return true, "deadline already passed"
			}
		}
	}

	// Backstop: too late to be anything but a replay.
	if delay := time.Since(ev.ScheduledAt); delay > s.staleEventCeiling {
		return true, "scheduled " + delay.Round(time.Hour).String() +
			" ago, beyond the " + s.staleEventCeiling.String() + " ceiling"
	}

	return false, ""
}

// checkUploadIntegrity reconciles media on disk against the rows that
// reference it and logs the outcome. Report-only — it never deletes.
func (s *Scheduler) checkUploadIntegrity(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.eventTimeout)
	defer cancel()

	if err := s.actions.CheckUploadIntegrity(ctx); err != nil {
		s.logger.ErrorContext(ctx, "Upload integrity check failed",
			slog.String("error", err.Error()))
	}
}

// checkpointWAL truncates the write-ahead log so it cannot grow without
// bound and so a bare copy of the database file stays complete.
func (s *Scheduler) checkpointWAL(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.eventTimeout)
	defer cancel()

	err := s.store.Checkpoint(ctx)
	if err == nil {
		return
	}

	// A reader holding an older snapshot is ordinary contention on a live
	// instance, not a fault: the next pass reclaims the frames.
	var busy *store.ErrCheckpointBusy
	if errors.As(err, &busy) {
		s.logger.WarnContext(ctx, "WAL checkpoint incomplete, retrying next pass",
			slog.Int("log_frames", busy.LogFrames),
			slog.Int("checkpointed_frames", busy.CheckpointedFrames),
		)

		return
	}

	s.logger.ErrorContext(ctx, "WAL checkpoint failed",
		slog.String("error", err.Error()))
}

type schedulerError string

func (e schedulerError) Error() string { return string(e) }

func errInvalidEvent(msg string) error {
	return schedulerError("invalid scheduler event: " + msg)
}
