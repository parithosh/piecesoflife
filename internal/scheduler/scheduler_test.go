package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestStore returns a fully-migrated Store backed by a fresh SQLite file,
// with the production pool shape (single write connection).
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	ctx := context.Background()

	st, err := store.New(ctx, filepath.Join(t.TempDir(), "test.db"), discardLogger())
	require.NoError(t, err, "open test store")
	require.NoError(t, st.RunMigrations(ctx), "run migrations")

	t.Cleanup(func() { _ = st.Close() })

	return st
}

// stubActions satisfies Actions with no-ops.
type stubActions struct{}

func (stubActions) SendReminderForIssue(context.Context, int64, bool, *int64) error {
	return nil
}

func (stubActions) SendAdminSummaryForIssue(context.Context, int64, *int64) error {
	return nil
}

func (stubActions) AutoPublishIssue(context.Context, int64) error { return nil }

func (stubActions) CreateNextIssue(context.Context, int64, time.Time) error {
	return nil
}

func (stubActions) ReconcileAutoCreate(context.Context) error { return nil }

func (stubActions) SendCommentDigests(context.Context) error { return nil }

// blockingDigestActions hangs SendCommentDigests until its context dies —
// the shape of a wedged store call or email send inside an event handler.
type blockingDigestActions struct {
	stubActions
}

func (blockingDigestActions) SendCommentDigests(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestSchedulerSurvivesConcurrentWrites guards the 2026-08-10 incident
// shape: the nightly cleanup events must complete while request-path
// writes hammer the single-connection write pool. Run with -race; a wedge
// fails via the Eventually deadline instead of hanging the suite.
func TestSchedulerSurvivesConcurrentWrites(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute).UTC()
	for _, eventType := range []string{
		"token_cleanup", "session_cleanup", "comment_digest",
	} {
		require.NoError(t, st.EnsureDailyEvent(ctx, eventType, past))
	}

	s := New(st, stubActions{}, discardLogger())
	s.tickInterval = 25 * time.Millisecond

	// Request-path writers: the login flow's write + read pair. They run
	// on a cancellable context so a wedge (writers parked in pool
	// acquisition) unblocks on teardown instead of deadlocking the test.
	writerCtx, cancelWriters := context.WithCancel(context.Background())
	writeErrs := make(chan error, 8)

	var wg sync.WaitGroup

	for w := range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := 0; writerCtx.Err() == nil; i++ {
				hash := fmt.Sprintf("hash-%d-%d", w, i)
				if err := st.RecordLoginAttempt(writerCtx, hash); err != nil {
					if writerCtx.Err() == nil {
						writeErrs <- err
					}

					return
				}

				if _, err := st.CountRecentLoginAttempts(writerCtx, hash); err != nil {
					if writerCtx.Err() == nil {
						writeErrs <- err
					}

					return
				}
			}
		}()
	}

	// Deferred so it also runs when require.Eventually fails below —
	// FailNow unwinds through defers, and the cancelled context frees
	// any writer still parked in pool acquisition.
	defer func() {
		cancelWriters()
		wg.Wait()
		s.Stop()

		close(writeErrs)
		for err := range writeErrs {
			require.NoError(t, err, "request-path write failed while scheduler ran")
		}
	}()

	s.Start(ctx)

	require.Eventually(t, func() bool {
		overdue, err := st.GetOverdueEvents(ctx)
		if err != nil {
			return false
		}

		return len(overdue) == 0
	}, 15*time.Second, 20*time.Millisecond,
		"scheduler wedged: overdue events never drained")
}

// TestSchedulerStuckEventDoesNotStopLoop verifies the per-event timeout:
// one hanging event handler must log-and-release within eventTimeout so
// later events still fire, instead of wedging the loop forever.
func TestSchedulerStuckEventDoesNotStopLoop(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// comment_digest is scheduled earlier, so it fires (and hangs) first.
	require.NoError(t, st.EnsureDailyEvent(
		ctx, "comment_digest", time.Now().Add(-2*time.Minute).UTC()))
	require.NoError(t, st.EnsureDailyEvent(
		ctx, "token_cleanup", time.Now().Add(-time.Minute).UTC()))

	s := New(st, blockingDigestActions{}, discardLogger())
	s.tickInterval = 25 * time.Millisecond
	s.eventTimeout = 100 * time.Millisecond

	s.Start(ctx)
	defer s.Stop()

	require.Eventually(t, func() bool {
		overdue, err := st.GetOverdueEvents(ctx)
		if err != nil {
			return false
		}

		cleanupDone := true
		digestPending := false

		for _, ev := range overdue {
			switch ev.EventType {
			case "token_cleanup":
				cleanupDone = false
			case "comment_digest":
				digestPending = true
			}
		}

		return cleanupDone && digestPending
	}, 10*time.Second, 20*time.Millisecond,
		"loop did not continue past the stuck comment_digest event")
}
