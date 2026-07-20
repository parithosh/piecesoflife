package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnsureDailyEventDedupes pins the fix for duplicate daily events:
// UNIQUE(issue_id, event_type, scheduled_at) never held for issue-less rows
// because SQLite treats NULLs as distinct inside UNIQUE constraints, so the
// old plain INSERT stacked a duplicate on every scheduler tick. Dedupe now
// comes from EnsureDailyEvent's INSERT OR IGNORE against the partial unique
// index added in migration 020.
func TestEnsureDailyEventDedupes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	midnight := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

	for range 3 {
		require.NoError(t, s.EnsureDailyEvent(ctx, "comment_digest", midnight))
	}

	assert.Equal(t, 1, countPendingByType(t, s, "comment_digest"),
		"repeat scheduling must not stack duplicate daily events")

	// A different midnight is a different event — the index must not
	// collapse consecutive days into one.
	require.NoError(t, s.EnsureDailyEvent(
		ctx, "comment_digest", midnight.AddDate(0, 0, 1)))

	assert.Equal(t, 2, countPendingByType(t, s, "comment_digest"),
		"the next day's event must still be schedulable")
}

// TestApplyScheduleRollsBackEventReplacement proves the schedule editor's
// multi-row writes are all-or-nothing. A replacement insert that fails must
// restore the old deadline/event, and a failed next-round re-pin must restore
// its old event, draft dates, and answering-window setting.
func TestApplyScheduleRollsBackEventReplacement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	oldDeadline := now.AddDate(0, 0, 7)
	issueID, err := s.CreateIssue(ctx, 1, nil, int(now.Month()), now.Year(), now, oldDeadline)
	require.NoError(t, err)
	require.NoError(t, s.SetIssueStatus(ctx, issueID, "collecting"))
	require.NoError(t, s.CreateSchedulerEvent(ctx, &issueID, "auto_close", oldDeadline))

	newDeadline := now.AddDate(0, 0, 10)
	_, err = s.ApplySchedule(ctx, ScheduleUpdate{
		GroupID:         1,
		CurrentIssueID:  &issueID,
		CurrentDeadline: &newDeadline,
		CurrentEvents: []SchedulerEventSpec{
			{EventType: "auto_close", ScheduledAt: newDeadline},
			// The CHECK constraint fails after the old event was deleted.
			{EventType: "not_an_event", ScheduledAt: newDeadline},
		},
	})
	require.Error(t, err)

	unchanged, err := s.GetIssueByID(ctx, issueID)
	require.NoError(t, err)
	assert.True(t, unchanged.Deadline.Equal(oldDeadline),
		"failed replacement rolls the deadline back")

	pending, err := s.GetPendingEvents(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "auto_close", pending[0].EventType)
	assert.True(t, pending[0].ScheduledAt.Equal(oldDeadline),
		"failed replacement restores the original event")

	oldOpen := now.AddDate(0, 1, 0)
	oldClose := oldOpen.AddDate(0, 0, 7)
	draftID, err := s.CreateIssue(ctx, 1, nil,
		int(oldOpen.Month()), oldOpen.Year(), oldOpen, oldClose)
	require.NoError(t, err)
	require.NoError(t, s.CreateSchedulerEvent(ctx, &draftID,
		"create_next_issue", oldOpen))

	newOpen := oldOpen.AddDate(0, 0, 3)
	newClose := newOpen.AddDate(0, 0, 10)
	// A fired historical row at the requested instant forces the replacement
	// insert to violate UNIQUE(issue_id, event_type, scheduled_at).
	require.NoError(t, s.CreateSchedulerEvent(ctx, &draftID,
		"create_next_issue", newOpen))
	events, err := s.GetPendingEvents(ctx)
	require.NoError(t, err)
	var historicalID int64
	for _, ev := range events {
		if ev.IssueID != nil && *ev.IssueID == draftID && ev.ScheduledAt.Equal(newOpen) {
			historicalID = ev.ID
		}
	}
	require.NotZero(t, historicalID)
	require.NoError(t, s.MarkEventFired(ctx, historicalID, false))

	settingsBefore, err := s.GetSettings(ctx, 1)
	require.NoError(t, err)
	_, err = s.ApplySchedule(ctx, ScheduleUpdate{
		GroupID:    1,
		NextOpen:   &newOpen,
		NextClose:  &newClose,
		NextMonth:  int(newOpen.Month()),
		NextYear:   newOpen.Year(),
		WindowDays: 10,
	})
	require.Error(t, err)

	draft, err := s.GetIssueByID(ctx, draftID)
	require.NoError(t, err)
	assert.True(t, draft.OpensAt.Equal(oldOpen), "failed re-pin restores the draft open")
	assert.True(t, draft.Deadline.Equal(oldClose), "failed re-pin restores the draft close")

	settingsAfter, err := s.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, settingsBefore.SubmissionWindowDays, settingsAfter.SubmissionWindowDays,
		"failed re-pin restores the answering window")

	pending, err = s.GetPendingEvents(ctx)
	require.NoError(t, err)
	foundOldOpen := false
	for _, ev := range pending {
		if ev.IssueID != nil && *ev.IssueID == draftID &&
			ev.EventType == "create_next_issue" && ev.ScheduledAt.Equal(oldOpen) {
			foundOldOpen = true
		}
	}
	assert.True(t, foundOldOpen, "failed re-pin restores the original queued opening")
}

// TestRequeueIssueEventsReArmsFiredRows pins the launch wizard's retry
// contract: replacing an issue's lifecycle must survive a collision with an
// already-fired row at the same (issue_id, event_type, scheduled_at).
// Deleting fired rows is not an option — email_log references them — so the
// requeue re-arms the colliding row in place instead of failing the UNIQUE
// constraint, while stale pending rows from the earlier attempt are dropped.
func TestRequeueIssueEventsReArmsFiredRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	deadline := now.AddDate(0, 0, 7)
	issueID, err := s.CreateIssue(ctx, 1, nil,
		int(now.Month()), now.Year(), now, deadline)
	require.NoError(t, err)

	// A previous wizard attempt queued the lifecycle; its auto_close fired,
	// and a pending reminder was left behind.
	require.NoError(t, s.CreateSchedulerEvent(ctx, &issueID, "auto_close", deadline))
	require.NoError(t, s.CreateSchedulerEvent(ctx, &issueID,
		"reminder_1", now.AddDate(0, 0, 4)))

	pending, err := s.GetPendingEvents(ctx)
	require.NoError(t, err)

	for _, ev := range pending {
		if ev.EventType == "auto_close" {
			require.NoError(t, s.MarkEventFired(ctx, ev.ID, false))
		}
	}

	// The retry re-submits the identical schedule.
	require.NoError(t, s.RequeueIssueEvents(ctx, issueID, []SchedulerEventSpec{
		{EventType: "auto_close", ScheduledAt: deadline},
		{EventType: "reminder_2", ScheduledAt: now.AddDate(0, 0, 6)},
	}))

	assert.Equal(t, 1, countPendingByType(t, s, "auto_close"),
		"a fired auto_close at the same instant is re-armed, not a constraint failure")
	assert.Equal(t, 1, countPendingByType(t, s, "reminder_2"))
	assert.Equal(t, 0, countPendingByType(t, s, "reminder_1"),
		"stale pending events from the earlier attempt are dropped")
}

// countPendingByType counts unfired scheduler events of one type.
func countPendingByType(t *testing.T, s *Store, eventType string) int {
	t.Helper()

	pending, err := s.GetPendingEvents(context.Background())
	require.NoError(t, err)

	count := 0

	for _, ev := range pending {
		if ev.EventType == eventType {
			count++
		}
	}

	return count
}
