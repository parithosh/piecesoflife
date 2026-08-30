package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

// recordingActions counts the member-visible actions a dispatch performed,
// so a test can assert an email was never sent rather than merely that no
// error occurred.
type recordingActions struct {
	stubActions

	reminders atomic.Int32
	publishes atomic.Int32
	creates   atomic.Int32
	summaries atomic.Int32
}

func (r *recordingActions) SendReminderForIssue(
	context.Context, int64, bool, *int64,
) error {
	r.reminders.Add(1)

	return nil
}

func (r *recordingActions) AutoPublishIssue(context.Context, int64) error {
	r.publishes.Add(1)

	return nil
}

func (r *recordingActions) CreateNextIssue(
	context.Context, int64, time.Time,
) error {
	r.creates.Add(1)

	return nil
}

func (r *recordingActions) SendAdminSummaryForIssue(
	context.Context, int64, *int64,
) error {
	r.summaries.Add(1)

	return nil
}

// newStaleEventFixture returns a scheduler over a fresh store plus a
// collecting issue whose deadline sits in the future.
func newStaleEventFixture(
	t *testing.T, actions Actions,
) (*Scheduler, *store.Store, int64) {
	t.Helper()

	ctx := context.Background()
	st := newTestStore(t)

	require.NoError(t, st.SeedDefaultGroup(ctx))

	now := time.Now().UTC()

	issueID, err := st.CreateIssue(
		ctx, 1, nil, 7, 2026, now.Add(-24*time.Hour), now.Add(24*time.Hour),
	)
	require.NoError(t, err)

	return New(st, actions, discardLogger()), st, issueID
}

// setCollecting moves an issue into the open state its scheduled events
// assume. CreateIssue leaves a draft.
func setCollecting(t *testing.T, st *store.Store, issueID int64) {
	t.Helper()

	ctx := context.Background()

	issue, err := st.GetIssueByID(ctx, issueID)
	require.NoError(t, err)

	require.NoError(t, st.OpenDraftEarly(
		ctx, issue.GroupID, issueID, false, nil,
		issue.Month, issue.Year, issue.OpensAt, issue.Deadline, nil,
	))
}

// TestReminderSkippedWhenIssueNoLongerCollecting is the confusing-email bug:
// members were asked to fill in a round they had already completed. The
// semantic guard catches it regardless of how late the event is — here the
// event is only a minute overdue.
func TestReminderSkippedWhenIssueNoLongerCollecting(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	// Round already published — a reminder for it is nonsense.
	require.NoError(t, st.PublishIssue(ctx, issueID))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.reminders.Load(),
		"no reminder may be sent for a published round")

	// Skipped, not left pending: it must never retry.
	remaining, err := st.GetOverdueEvents(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "skipped event must be marked fired")
}

// TestAutoCloseSkippedWhenAlreadyPublished guards the second half of the
// cascade: the stale auto_close that republished July and mailed everyone.
func TestAutoCloseSkippedWhenAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	require.NoError(t, st.PublishIssue(ctx, issueID))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "auto_close", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.publishes.Load(),
		"an already-published round must not be published again")
}

// TestReminderSkippedBeyondStalenessCeiling covers the backstop: the round
// is still legitimately collecting, but the event is weeks late — the
// fingerprint of a restored database replaying history.
func TestReminderSkippedBeyondStalenessCeiling(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-23*24*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Zero(t, actions.reminders.Load(),
		"a 23-day-late reminder must not reach members")
}

// TestReminderFiresWhenMerelyDelayed is the guard against over-blocking: a
// short outage must still catch up, which is the whole point of persisting
// events.
func TestReminderFiresWhenMerelyDelayed(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-20*time.Minute),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Equal(t, int32(1), actions.reminders.Load(),
		"a 20-minute-late reminder is a normal restart catch-up")
}

// TestCleanupEventStillCatchesUpWhenVeryLate keeps maintenance events out of
// the ceiling: they are idempotent and touch nobody's inbox.
func TestCleanupEventStillCatchesUpWhenVeryLate(t *testing.T) {
	ctx := context.Background()
	sched, st, _ := newStaleEventFixture(t, stubActions{})

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, nil, "token_cleanup", time.Now().UTC().Add(-30*24*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	remaining, err := st.GetOverdueEvents(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cleanup must run, however late")
}

// TestUnreadableIssueIsNotSkipped is the guard against the gate eating work
// it could not verify. Skipping marks an event fired and consumes it
// forever, so a failed issue lookup must not be grounds for it — not even
// past the staleness ceiling, where the temporal backstop would otherwise
// fire blind on an event that might be perfectly legitimate.
//
// A dangling issue_id cannot be inserted (foreign keys forbid it), so the
// unreadable case is produced the way it actually occurs in production: the
// store call fails.
func TestUnreadableIssueIsNotSkipped(t *testing.T) {
	sched, _, issueID := newStaleEventFixture(t, &recordingActions{})

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	ev := store.SchedulerEvent{
		ID:          1,
		IssueID:     &issueID,
		EventType:   "reminder_1",
		ScheduledAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
	}

	skip, reason := sched.shouldSkipEvent(dead, ev)

	assert.False(t, skip,
		"an event we could not revalidate must not be skipped, however late")
	assert.Empty(t, reason)
}

// TestCreateNextIssueSkippedWhenAlreadyOpened covers the fifth gated event
// type: the event opens a pre-created draft, so anything else means the
// round is already open and reopening it would rewind a live round.
func TestCreateNextIssueSkippedWhenAlreadyOpened(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "create_next_issue", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.creates.Load(),
		"a round that is already collecting must not be opened again")
}

// TestAdminSummaryFiresAfterDeadlineWhileCollecting pins the deliberate
// asymmetry: admin_summary is scheduled at the same hour as reminder_2 but
// deliberately has no deadline test, because the admin still wants the
// round's numbers while it is closing.
func TestAdminSummaryFiresAfterDeadlineWhileCollecting(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "admin_summary", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Equal(t, int32(1), actions.summaries.Load(),
		"admin summary must still reach the admin of a collecting round")
}
