package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminScheduleEditor locks in the dashboard schedule editor's contract:
// an admin who started a round at the wrong cadence can move the live
// round's close date and pin exactly when the next round opens and closes
// (e.g. "opens the 15th, closes the 25th"). Pinning pre-creates the next
// draft with a queued create_next_issue event, persists the implied
// answering window, and survives publish without a duplicate cadence event.
func TestAdminScheduleEditor(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	// pendingCreateNext returns the unfired create_next_issue events.
	pendingCreateNext := func() []time.Time {
		events, err := env.store.GetPendingEvents(ctx)
		require.NoError(t, err)

		var out []time.Time

		for _, ev := range events {
			if ev.EventType == "create_next_issue" {
				out = append(out, ev.ScheduledAt)
			}
		}

		return out
	}

	// A round opened today on the wrong cadence (default 7-day window).
	createRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Wrong cadence"}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	// The next round can't open before the current one closes.
	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(2), day(12)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"next round overlapping the live one must be rejected: %s", rr.Body.String())

	// The answering window must stay between 3 and 21 days.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(20), day(22)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"2-day window must be rejected: %s", rr.Body.String())

	// Move the live round's close and pin the next round: close in 6 days,
	// next round opens in 20 and closes in 30 — a 10-day window.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q, "next_open_date": %q, "next_close_date": %q}`,
			issue.ID, day(6), day(20), day(30)))))
	require.Equal(t, http.StatusOK, rr.Code, "schedule: %s", rr.Body.String())

	// Moving only the live round's close past the pinned next open must be
	// rejected — accepted, the pinned open event would fire mid-collection,
	// be consumed as a no-op, and silently unpin the schedule.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q}`, issue.ID, day(21)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"close past the pinned open must be rejected: %s", rr.Body.String())

	// Older clients can still call both pre-editor deadline APIs. They must
	// enforce the same bound or the queued opening will fire while this
	// round is collecting, no-op, and silently consume the admin's pin.
	overlapDay, err := time.Parse("2006-01-02", day(21))
	require.NoError(t, err)
	overlapDeadline := overlapDay.Add(21 * time.Hour).Format(time.RFC3339)

	rr = env.do(t, authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/extend", issue.ID),
		fmt.Sprintf(`{"new_deadline": %q}`, overlapDeadline))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"legacy extend must preserve the pinned opening: %s", rr.Body.String())

	rr = env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/issues/%d", issue.ID),
		fmt.Sprintf(`{"deadline": %q}`, overlapDeadline))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"generic issue update must preserve the pinned opening: %s", rr.Body.String())

	// A save pinned to a round that is no longer the live one must 409
	// instead of silently rescheduling whichever round is collecting now.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q}`, issue.ID+1000, day(5)))))
	assert.Equal(t, http.StatusConflict, rr.Code,
		"stale dialog must not reschedule the current round: %s", rr.Body.String())

	// Live round: deadline lands at 9 PM local on the chosen date and the
	// auto_close event follows it.
	updated, err := env.store.GetIssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, day(6), updated.Deadline.In(time.UTC).Format("2006-01-02"),
		"live round closes on the chosen date")
	assert.Equal(t, 21, updated.Deadline.In(time.UTC).Hour(),
		"close pinned to the friendly evening hour")

	events, err := env.store.GetPendingEvents(ctx)
	require.NoError(t, err)

	foundAutoClose := false

	for _, ev := range events {
		if ev.EventType == "auto_close" && ev.IssueID != nil && *ev.IssueID == issue.ID {
			foundAutoClose = true

			assert.True(t, ev.ScheduledAt.Equal(updated.Deadline),
				"auto_close follows the moved deadline")
		}
	}

	require.True(t, foundAutoClose, "moved round keeps its auto_close event")

	// The implied 10-day window becomes the loop's rhythm.
	settings, err := env.store.GetSettings(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 10, settings.SubmissionWindowDays,
		"pinned dates persist the answering window")

	// Next round: pre-created draft + queued open event on the pinned dates.
	draft, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft, "pinning pre-creates the next round as a draft")
	assert.Equal(t, day(20), draft.OpensAt.In(time.UTC).Format("2006-01-02"))
	assert.Equal(t, 9, draft.OpensAt.In(time.UTC).Hour(), "rounds open mid-morning")
	assert.Equal(t, day(30), draft.Deadline.In(time.UTC).Format("2006-01-02"))
	assert.Equal(t, 21, draft.Deadline.In(time.UTC).Hour())

	// The masthead label is the open month — unless the live round already
	// owns that (month, year), in which case the draft walks forward to the
	// first free label so the /issues/{year}/{month} archive can address
	// both editions.
	openDay, err := time.Parse("2006-01-02", day(20))
	require.NoError(t, err)

	wantMonth, wantYear := int(openDay.Month()), openDay.Year()
	if wantMonth == issue.Month && wantYear == issue.Year {
		wantMonth++
		if wantMonth == 13 {
			wantMonth, wantYear = 1, wantYear+1
		}
	}

	assert.Equal(t, wantMonth, draft.Month, "draft labeled with the first free month")
	assert.Equal(t, wantYear, draft.Year)

	queued := pendingCreateNext()
	require.Len(t, queued, 1, "exactly one open event queued")
	assert.True(t, queued[0].Equal(draft.OpensAt), "open event fires when the draft opens")

	// Re-pinning moves the same draft instead of stacking a second round.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(21), day(31)))))
	require.Equal(t, http.StatusOK, rr.Code, "re-pin: %s", rr.Body.String())

	repinned, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, repinned)
	assert.Equal(t, draft.ID, repinned.ID, "re-pin reuses the queued draft")
	assert.Equal(t, day(21), repinned.OpensAt.In(time.UTC).Format("2006-01-02"))

	queued = pendingCreateNext()
	require.Len(t, queued, 1, "re-pin never duplicates the open event")
	assert.True(t, queued[0].Equal(repinned.OpensAt))

	// Publishing the live round keeps the pinned schedule: the publish-time
	// cadence queueing must back off instead of stacking a second event.
	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issue.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	queued = pendingCreateNext()
	require.Len(t, queued, 1, "publish keeps the single pinned open event")
	assert.True(t, queued[0].Equal(repinned.OpensAt),
		"publish leaves the admin's pinned open time untouched")

	// A stale open event — one pointing at a round that already opened
	// (started manually before its event fired, or legacy data) — must be
	// purged by a re-pin: left alone it would fire later and open the
	// pinned draft off its date.
	require.NoError(t, env.store.CreateSchedulerEvent(ctx, &issue.ID,
		"create_next_issue", time.Now().Add(24*time.Hour)))

	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(22), day(32)))))
	require.Equal(t, http.StatusOK, rr.Code, "re-pin over stale: %s", rr.Body.String())

	queued = pendingCreateNext()
	require.Len(t, queued, 1, "stale open events are purged when the admin pins")

	final, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, final)
	assert.True(t, queued[0].Equal(final.OpensAt),
		"the surviving event is the pinned draft's")

	// The pin re-anchors the ENTIRE lifecycle, not one round: run the
	// pinned round through a full cycle (open → publish) and the round
	// after it must inherit the rhythm — same cadence slot derived from
	// the pinned open, same persisted window. No further admin input.
	require.NoError(t, env.srv.CreateNextIssue(ctx, 1), "open the pinned round")

	pinnedRound, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, final.ID, pinnedRound.ID, "the pinned draft is what opened")
	assert.True(t, pinnedRound.OpensAt.Equal(final.OpensAt),
		"opening keeps the pinned anchor date")

	pubRR = env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", pinnedRound.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish pinned round: %s", pubRR.Body.String())

	heir, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, heir, "publish pre-creates the following round")

	// Monthly cadence from the pinned anchor: one month after the pinned
	// open, same mid-morning hour, closing after the persisted 10-day
	// window at the friendly evening hour.
	wantOpen := pinnedRound.OpensAt.AddDate(0, 1, 0)
	assert.True(t, heir.OpensAt.Equal(wantOpen),
		"round after the pinned one opens a month after the new anchor: got %v want %v",
		heir.OpensAt, wantOpen)

	wantClose := heir.OpensAt.In(time.UTC).AddDate(0, 0, 10)
	assert.Equal(t, wantClose.Format("2006-01-02"),
		heir.Deadline.In(time.UTC).Format("2006-01-02"),
		"inherited deadline honors the persisted 10-day window")
	assert.Equal(t, 21, heir.Deadline.In(time.UTC).Hour())

	heirEvent, err := env.store.GetPendingNextRoundEvent(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, heirEvent, "the inherited round is queued to open itself")
	assert.True(t, heirEvent.ScheduledAt.Equal(heir.OpensAt))

	// "Start it now" on the pre-created heir must consume its queued open
	// event — left pending, it would fire weeks later and open the round
	// after next off schedule — and must not relabel the heir onto a
	// (month, year) another edition already uses.
	startRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, startRR.Code, "start now: %s", startRR.Body.String())

	assert.Empty(t, pendingCreateNext(), "early open consumes the queued open event")

	opened, err := env.store.GetIssueByID(ctx, heir.ID)
	require.NoError(t, err)
	assert.Equal(t, "collecting", opened.Status, "the heir draft is what opened")

	allIssues, err := env.store.ListIssues(ctx, 1, nil)
	require.NoError(t, err)

	labels := make(map[string]int, len(allIssues))
	for _, iss := range allIssues {
		labels[fmt.Sprintf("%d-%02d", iss.Year, iss.Month)]++
	}

	for label, n := range labels {
		assert.Equal(t, 1, n, "issue label %s must be unique", label)
	}
}

// TestScheduleEditorCalendarBounds locks validation to calendar days in the
// loop's timezone: every date the dashboard's pickers advertise must save
// whatever the wall clock says — the pinned 9 AM/9 PM instants must not
// disqualify the boundary days for most of the day.
func TestScheduleEditorCalendarBounds(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	// The pickers' outermost advertised dates — close at today+90, next
	// open at today+180 — must be accepted at any time of day.
	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q, "next_open_date": %q, "next_close_date": %q}`,
			issue.ID, day(90), day(180), day(190)))))
	require.Equal(t, http.StatusOK, rr.Code,
		"the pickers' own max dates must save: %s", rr.Body.String())

	// Just past the advertised maxima is rejected. The one-day margin keeps
	// the assertions stable across a midnight crossing mid-test.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q}`, issue.ID, day(92)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"a close past 90 days out must be rejected: %s", rr.Body.String())

	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(182), day(192)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"a next open past 6 months out must be rejected: %s", rr.Body.String())

	// The next round can never open today — its 9 AM instant has usually
	// passed already; "Start it now" is the immediate path.
	rr = env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(0), day(10)))))
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"a next open today must be rejected: %s", rr.Body.String())
}

// TestScheduleSuggestionClearsCurrentClose pins the dialog prefill contract:
// when the live round's close was pushed past the cadence slot, the
// suggested next open must slide past the close — a prefill the server
// rejects on sight helps nobody.
func TestScheduleSuggestionClearsCurrentClose(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	// Push the close well past the monthly cadence slot (~a month out).
	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q}`, issue.ID, day(40)))))
	require.Equal(t, http.StatusOK, rr.Code, "move close: %s", rr.Body.String())

	settings, err := env.store.GetSettings(ctx, 1)
	require.NoError(t, err)

	current, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	view := env.srv.nextRoundView(ctx, settings, current)
	require.NotNil(t, view, "cadence suggestion exists while a round collects")
	assert.False(t, view.Queued)
	assert.Equal(t, day(41), view.OpenDate,
		"suggestion slides to the morning after the pushed-out close")
}

// TestIssuePatchDraftAndCombined restores the pre-editor contract that
// PATCH /api/issues/{id} can adjust an upcoming draft's deadline, and pins
// the combined title+deadline update applying together on a collecting
// round — never the deadline (and everyone's reminder emails) alone behind
// an error response.
func TestIssuePatchDraftAndCombined(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(20), day(30)))))
	require.Equal(t, http.StatusOK, rr.Code, "pin next round: %s", rr.Body.String())

	draft, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft)

	// The upcoming draft's deadline can be adjusted directly — it has no
	// reminder events to move, and the queued opening tracks the open date.
	newClose := draft.OpensAt.AddDate(0, 0, 12).Truncate(time.Second)
	rr = env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/issues/%d", draft.ID),
		fmt.Sprintf(`{"deadline": %q}`, newClose.Format(time.RFC3339)))))
	require.Equal(t, http.StatusOK, rr.Code,
		"a draft's deadline must be adjustable: %s", rr.Body.String())

	updatedDraft, err := env.store.GetIssueByID(ctx, draft.ID)
	require.NoError(t, err)
	assert.True(t, updatedDraft.Deadline.Equal(newClose), "draft deadline moved")
	assert.Equal(t, "draft", updatedDraft.Status)

	ev, err := env.store.GetPendingNextRoundEvent(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ev, "the queued opening survives a draft deadline change")
	assert.True(t, ev.ScheduledAt.Equal(draft.OpensAt))

	// Collecting round: title and deadline land together.
	combined := time.Now().UTC().AddDate(0, 0, 5).Truncate(time.Second)
	rr = env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/issues/%d", issue.ID),
		fmt.Sprintf(`{"title": "Renamed", "deadline": %q}`, combined.Format(time.RFC3339)))))
	require.Equal(t, http.StatusOK, rr.Code, "combined patch: %s", rr.Body.String())

	updated, err := env.store.GetIssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Title)
	assert.Equal(t, "Renamed", *updated.Title)
	assert.True(t, updated.Deadline.Equal(combined))

	// A published round's schedule stays closed.
	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issue.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	rr = env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/issues/%d", issue.ID),
		fmt.Sprintf(`{"deadline": %q}`, combined.AddDate(0, 0, 2).Format(time.RFC3339)))))
	assert.Equal(t, http.StatusConflict, rr.Code,
		"a published round's deadline must not change: %s", rr.Body.String())
}

// TestShortenedDeadlineSendsLastChance pins the promise the dialog makes:
// when the close moves too near for any standard reminder slot, members who
// haven't answered get an immediate last-chance reminder — even though a
// reminder fired within the last day, because that one advertised the old,
// later deadline.
func TestShortenedDeadlineSendsLastChance(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	// A routine reminder went out an hour ago, advertising the current
	// close a week out.
	require.NoError(t, env.store.CreateSchedulerEvent(ctx, &issue.ID,
		"reminder_1", time.Now().UTC().Add(-1*time.Hour)))

	pending, err := env.store.GetPendingEvents(ctx)
	require.NoError(t, err)

	for _, ev := range pending {
		if ev.EventType == "reminder_1" && ev.ScheduledAt.Before(time.Now()) {
			require.NoError(t, env.store.MarkEventFired(ctx, ev.ID, false))
		}
	}

	// Pull the close in to tomorrow — no standard slot fits any more.
	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"current_issue_id": %d, "current_close_date": %q}`, issue.ID, day(1)))))
	require.Equal(t, http.StatusOK, rr.Code, "shorten close: %s", rr.Body.String())

	pending, err = env.store.GetPendingEvents(ctx)
	require.NoError(t, err)

	foundImmediate := false

	for _, ev := range pending {
		if ev.EventType == "reminder_2" && ev.IssueID != nil && *ev.IssueID == issue.ID {
			foundImmediate = true

			assert.WithinDuration(t, time.Now(), ev.ScheduledAt, 5*time.Minute,
				"the last-chance reminder fires immediately")
		}
	}

	require.True(t, foundImmediate,
		"a shortened close must queue an immediate last-chance reminder even after a recent one")
}

// TestReconcileRepairsStalledLoop pins the reconciler's reach: a Loop whose
// queued opening was lost — and even one whose draft's open date already
// slipped past — must be repaired by ReconcileAutoCreate rather than read
// as "active" and skipped forever.
func TestReconcileRepairsStalledLoop(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(20), day(30)))))
	require.Equal(t, http.StatusOK, rr.Code, "pin next round: %s", rr.Body.String())

	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issue.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	draft, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft)

	// Sabotage 1: the queued opening vanishes (an event insert failure at
	// publish time). The future-dated draft must get its event back.
	_, err = env.store.DeletePendingGroupEventsByType(ctx, 1, "create_next_issue")
	require.NoError(t, err)

	require.NoError(t, env.srv.ReconcileAutoCreate(ctx))

	ev, err := env.store.GetPendingNextRoundEvent(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ev, "reconcile re-queues a future draft's lost opening")
	assert.True(t, ev.ScheduledAt.Equal(draft.OpensAt), "the draft keeps its pinned date")

	// Sabotage 2: the draft's open date also slipped past (server down for
	// days). HasActiveIssue reads this as an active round — the reconciler
	// must not, or the loop stays dead until a publish that never comes.
	_, err = env.store.DeletePendingGroupEventsByType(ctx, 1, "create_next_issue")
	require.NoError(t, err)

	staleOpen := time.Now().UTC().AddDate(0, 0, -3)
	require.NoError(t, env.store.UpdateIssueSchedule(ctx, draft.ID,
		draft.Month, draft.Year, staleOpen, staleOpen.AddDate(0, 0, 7)))

	require.NoError(t, env.srv.ReconcileAutoCreate(ctx))

	repaired, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, repaired)
	require.Equal(t, draft.ID, repaired.ID)
	assert.True(t, repaired.OpensAt.After(time.Now()),
		"the stalled draft is re-anchored to a future slot")

	ev, err = env.store.GetPendingNextRoundEvent(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ev, "the stalled draft's opening is re-queued")
	assert.True(t, ev.ScheduledAt.Equal(repaired.OpensAt))
}

// TestLateOpenReAnchorsStaleDraft pins the fire-time guard: a
// create_next_issue event that fires after its draft's own close (server
// down for days) must open the round with a fresh answering window — not a
// past deadline that auto-closes and publishes an empty round on the next
// scheduler tick.
func TestLateOpenReAnchorsStaleDraft(t *testing.T) {
	env, authed, day := newScheduleTestEnv(t)
	ctx := context.Background()

	createRR := env.do(t, authed(newJSONRequest(http.MethodPost, "/api/issues", `{}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	rr := env.do(t, authed(newJSONRequest(http.MethodPut, "/api/admin/schedule",
		fmt.Sprintf(`{"next_open_date": %q, "next_close_date": %q}`, day(20), day(30)))))
	require.Equal(t, http.StatusOK, rr.Code, "pin next round: %s", rr.Body.String())

	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issue.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	draft, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft)

	// The server slept through the draft's whole answering window.
	staleOpen := time.Now().UTC().AddDate(0, 0, -10)
	require.NoError(t, env.store.UpdateIssueSchedule(ctx, draft.ID,
		draft.Month, draft.Year, staleOpen, staleOpen.AddDate(0, 0, 7)))

	require.NoError(t, env.srv.CreateNextIssue(ctx, 1))

	opened, err := env.store.GetIssueByID(ctx, draft.ID)
	require.NoError(t, err)
	assert.Equal(t, "collecting", opened.Status, "the stale draft still opens")
	assert.True(t, opened.Deadline.After(time.Now().Add(24*time.Hour)),
		"the round opens with a fresh answering window, not a past deadline")

	foundAutoClose := false

	pending, err := env.store.GetPendingEvents(ctx)
	require.NoError(t, err)

	for _, ev := range pending {
		if ev.EventType == "auto_close" && ev.IssueID != nil && *ev.IssueID == draft.ID {
			foundAutoClose = true

			assert.True(t, ev.ScheduledAt.Equal(opened.Deadline),
				"auto_close follows the fresh deadline")
		}
	}

	require.True(t, foundAutoClose, "the re-anchored round keeps its auto_close")
}

// newScheduleTestEnv wires an integration env with an authenticated admin
// and a day(n) calendar-date formatter in the seeded loop timezone (UTC).
// The base time is captured once so submitted dates and later assertions
// agree even when the test run crosses UTC midnight.
func newScheduleTestEnv(t *testing.T) (
	*integrationEnv, func(*http.Request) *http.Request, func(int) string,
) {
	t.Helper()

	env := newIntegrationEnv(t)
	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	authed := func(req *http.Request) *http.Request {
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)

		return req
	}

	base := time.Now().UTC()
	day := func(n int) string {
		return base.AddDate(0, 0, n).Format("2006-01-02")
	}

	return env, authed, day
}
