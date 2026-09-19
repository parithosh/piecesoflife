package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

// recordingActions counts the member-visible actions a dispatch performed,
// so a test can assert an email was never sent rather than merely that no
// error occurred. Plain ints: every test here drives fireOverdueEvents
// synchronously, with no scheduler goroutine in play.
type recordingActions struct {
	stubActions

	reminders int
	publishes int
	creates   int
	summaries int
}

func (r *recordingActions) SendReminderForIssue(
	context.Context, int64, bool, *int64,
) error {
	r.reminders++

	return nil
}

func (r *recordingActions) AutoPublishIssue(context.Context, int64) error {
	r.publishes++

	return nil
}

func (r *recordingActions) CreateNextIssue(
	context.Context, int64, time.Time,
) error {
	r.creates++

	return nil
}

func (r *recordingActions) SendAdminSummaryForIssue(
	context.Context, int64, *int64,
) error {
	r.summaries++

	return nil
}

// newStaleEventFixture returns a scheduler over a fresh store plus a draft
// issue whose deadline sits a day in the future.
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

// setCollecting opens the draft into the state its scheduled events assume,
// keeping its existing window.
func setCollecting(t *testing.T, st *store.Store, issueID int64) {
	t.Helper()

	issue, err := st.GetIssueByID(context.Background(), issueID)
	require.NoError(t, err)

	setCollectingWithDeadline(t, st, issueID, issue.Deadline)
}

// setCollectingWithDeadline opens the draft with an explicit deadline, so a
// test can put the round's close in the past while it is still collecting.
func setCollectingWithDeadline(
	t *testing.T, st *store.Store, issueID int64, deadline time.Time,
) {
	t.Helper()

	ctx := context.Background()

	issue, err := st.GetIssueByID(ctx, issueID)
	require.NoError(t, err)

	require.NoError(t, st.OpenDraftEarly(
		ctx, issue.GroupID, issueID, false, nil,
		issue.Month, issue.Year, issue.OpensAt, deadline, nil,
	))
}

// TestReminderSkippedWhenIssueNoLongerCollecting is the confusing-email bug:
// members were asked to fill in a round they had already completed. State
// decides it, so the guard catches it however small the delay — here one
// minute, which no lateness ceiling would ever reject.
func TestReminderSkippedWhenIssueNoLongerCollecting(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	require.NoError(t, st.PublishIssue(ctx, issueID))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.reminders,
		"no reminder may be sent for a published round")

	// Skipped, not left pending: it must never retry.
	remaining, err := st.GetOverdueEvents(ctx)
	require.NoError(t, err)
	assert.Empty(t, remaining, "skipped event must be marked fired")
}

// TestReminderSkippedAfterDeadline is the branch that actually blocked the
// 2026-08-05 reminders: the round was still marked collecting in the stale
// database, but its deadline had passed weeks earlier.
func TestReminderSkippedAfterDeadline(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollectingWithDeadline(t, st, issueID, time.Now().UTC().Add(-48*time.Hour))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-23*24*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Zero(t, actions.reminders,
		"a reminder for a round whose deadline has passed must not be sent")
}

// TestReminderFiresWhenVeryLateButRoundStillOpen guards against
// over-blocking, which is why reminders are exempt from the lateness
// ceiling: a reminder queued at least minReminderLead (12h) ahead can be
// days late and still have most of the answering window left.
func TestReminderFiresWhenVeryLateButRoundStillOpen(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollectingWithDeadline(t, st, issueID, time.Now().UTC().Add(72*time.Hour))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-49*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Equal(t, 1, actions.reminders,
		"the round is open with days to go, so the reminder is still useful")
}

// TestReminderFiresWhenMerelyDelayed is the ordinary restart catch-up that
// persisting events exists for.
func TestReminderFiresWhenMerelyDelayed(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "reminder_1", time.Now().UTC().Add(-20*time.Minute),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Equal(t, 1, actions.reminders,
		"a 20-minute-late reminder is a normal restart catch-up")
}

// TestAutoCloseSkippedWhenAlreadyPublished covers the state half of the
// auto_close policy.
func TestAutoCloseSkippedWhenAlreadyPublished(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	require.NoError(t, st.PublishIssue(ctx, issueID))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "auto_close", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.publishes,
		"an already-published round must not be published again")
}

// TestAutoCloseSkippedBeyondCeiling is why the lateness ceiling still
// exists. This is the 2026-08-05 auto_close exactly: a restored database
// still says "collecting", so state alone cannot reject it, and firing it
// published July with one member's answers.
func TestAutoCloseSkippedBeyondCeiling(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "auto_close", time.Now().UTC().Add(-16*24*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Zero(t, actions.publishes,
		"a 16-day-late close is a replay, not a catch-up")
}

// TestAutoCloseFiresAtItsDeadline pins the other side of that ceiling: a
// round closing on schedule must still close.
func TestAutoCloseFiresAtItsDeadline(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "auto_close", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Equal(t, 1, actions.publishes, "a round due to close must close")
}

// TestCreateNextIssueFiresWhenVeryLateOnDraft is why create_next_issue is
// exempt from the ceiling. CreateNextIssue deliberately re-anchors an
// overdue draft to a fresh answering window (see
// TestLateOpenReAnchorsStaleDraft), so a week-late opening is work to do,
// not a replay to discard — skipping it would leave the Loop with no open
// round at all.
func TestCreateNextIssueFiresWhenVeryLateOnDraft(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "create_next_issue", time.Now().UTC().Add(-7*24*time.Hour),
	))

	sched.fireOverdueEvents(ctx, true)

	assert.Equal(t, 1, actions.creates,
		"a late opening must still open the round, however long the outage")
}

// TestCreateNextIssueSkippedWhenAlreadyOpened is its state check: anything
// but a draft means the round is already open.
func TestCreateNextIssueSkippedWhenAlreadyOpened(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollecting(t, st, issueID)

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "create_next_issue", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Zero(t, actions.creates,
		"a round that is already collecting must not be opened again")
}

// TestAdminSummaryFiresAfterDeadlineWhileCollecting pins the deliberate
// asymmetry with reminders: the admin still wants the round's numbers while
// it is closing, so admin_summary has no deadline test.
func TestAdminSummaryFiresAfterDeadlineWhileCollecting(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	setCollectingWithDeadline(t, st, issueID, time.Now().UTC().Add(-time.Hour))

	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "admin_summary", time.Now().UTC().Add(-time.Minute),
	))

	sched.fireOverdueEvents(ctx, false)

	assert.Equal(t, 1, actions.summaries,
		"admin summary must still reach the admin of a collecting round")
}

// TestCleanupEventStillCatchesUpWhenVeryLate keeps maintenance events out of
// the gate entirely: they are idempotent and touch nobody's inbox.
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
// past the staleness ceiling.
//
// A dangling issue_id cannot be inserted (foreign keys forbid it), so the
// unreadable case is produced the way it actually occurs: the store call
// fails.
func TestUnreadableIssueIsNotSkipped(t *testing.T) {
	sched, _, issueID := newStaleEventFixture(t, &recordingActions{})

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	ev := store.SchedulerEvent{
		ID:          1,
		IssueID:     &issueID,
		EventType:   "auto_close",
		ScheduledAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
	}

	skip, reason := sched.shouldSkipEvent(dead, ev)

	assert.False(t, skip,
		"an event we could not revalidate must not be skipped, however late")
	assert.Empty(t, reason)
}

// TestEventAlreadyFiredOnWriteSideIsNotReplayed is the 2026-09-18 incident:
// a pooled read connection frozen inside an old snapshot handed the loop a
// create_next_issue row that the write side had marked fired two weeks
// earlier, and the round was re-opened every tick. The overdue row here is
// built the way that reader delivered it — fired_at empty — while the
// store already holds it fired. The gate must consult the write side and
// act on nothing.
func TestEventAlreadyFiredOnWriteSideIsNotReplayed(t *testing.T) {
	ctx := context.Background()
	actions := &recordingActions{}
	sched, st, issueID := newStaleEventFixture(t, actions)

	scheduledAt := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, st.CreateSchedulerEvent(
		ctx, &issueID, "create_next_issue", scheduledAt,
	))

	overdue, err := st.GetOverdueEvents(ctx)
	require.NoError(t, err)
	require.Len(t, overdue, 1)

	require.NoError(t, st.MarkEventFired(ctx, overdue[0].ID, false))

	stale := overdue[0]
	stale.FiredAt = nil

	sched.fireEvent(ctx, stale, false)

	assert.Zero(t, actions.creates,
		"an event the write side has already fired must not run again")
}

// TestMarkEventFiredRejectsUnknownID keeps the mark from being a silent
// no-op: a row the writer cannot see is an error to surface, not success.
func TestMarkEventFiredRejectsUnknownID(t *testing.T) {
	_, st, _ := newStaleEventFixture(t, stubActions{})

	err := st.MarkEventFired(context.Background(), 424242, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such event")
}
