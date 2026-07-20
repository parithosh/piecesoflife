package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// scheduleDateLayout is the calendar-date wire format of the schedule
// editor. Dates are interpreted in the loop's timezone; the server pins the
// actual instants to the loop's friendly hours (rounds open at 9 AM local,
// close at 9 PM local), so admins never juggle timezones or clock times.
const scheduleDateLayout = "2006-01-02"

// maxCloseDays and maxNextOpenDays bound how far out the schedule editor
// can push dates (close within 90 days, next open within ~6 months) — a
// mistyped year must not silently park the loop for a year. The dashboard's
// date pickers mirror these as max attributes.
const (
	maxCloseDays    = 90
	maxNextOpenDays = 180
)

// updateScheduleRequest is the JSON body for PUT /api/admin/schedule. All
// fields are optional calendar dates (YYYY-MM-DD, loop timezone), but
// next_open_date and next_close_date must travel together — the answering
// window is derived from their distance. current_issue_id names the round
// the dialog was opened for and must accompany current_close_date: a stale
// dashboard tab must not silently reschedule whichever round is live now.
type updateScheduleRequest struct {
	CurrentIssueID   int64  `json:"current_issue_id"`
	CurrentCloseDate string `json:"current_close_date"`
	NextOpenDate     string `json:"next_open_date"`
	NextCloseDate    string `json:"next_close_date"`
}

// handleUpdateSchedule lets an admin re-anchor the loop's rhythm: move the
// live round's close date (either direction, as long as it stays in the
// future) and/or pin exactly when the next round opens and closes. Pinning
// the next round pre-creates it as a draft with a queued create_next_issue
// event and persists the implied answering window to settings — and because
// cadence math anchors on the previous round's open, every following round
// inherits the new rhythm (e.g. "opens the 15th, closes the 25th").
// PUT /api/admin/schedule
func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := currentGroupID(ctx)

	var req updateScheduleRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if req.CurrentCloseDate == "" && req.NextOpenDate == "" && req.NextCloseDate == "" {
		writeValidationError(w, map[string]string{
			"schedule": "Nothing to change — provide at least one date",
		})

		return
	}

	if (req.NextOpenDate == "") != (req.NextCloseDate == "") {
		writeValidationError(w, map[string]string{
			"next_open_date": "Provide both next-round dates, or neither",
		})

		return
	}

	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings for schedule update",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load settings")

		return
	}

	loc := s.settingsLocation(ctx, settings)
	now := time.Now()
	// Bounds are calendar days, not instants: the dialog's date pickers
	// advertise "today" through "today+N", and a date the picker offers must
	// not bounce off the server because the pinned 9 AM/9 PM instant has
	// already slipped past on the boundary day.
	nowLocal := now.In(loc)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
	fieldErrs := make(map[string]string, 3)

	// The live round, when one is collecting. currentDeadline tracks the
	// close the loop will actually have after this call, so the next round's
	// open can be validated against the round it follows.
	var current *store.Issue

	var currentDeadline time.Time

	active, aErr := s.store.GetActiveIssue(ctx, groupID)

	switch {
	case aErr == nil && active.Status == "collecting":
		current = active
		currentDeadline = active.Deadline
	case aErr != nil && !errors.Is(aErr, sql.ErrNoRows):
		// A store fault must not read as "no active round" — it would
		// return a misleading 409 here, or skip the current-vs-next
		// overlap validation below entirely.
		s.logger.ErrorContext(ctx, "Failed to look up the active round for schedule update",
			slog.String("error", aErr.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to look up the current round")

		return
	}

	var newCurrentClose time.Time

	if req.CurrentCloseDate != "" {
		if current == nil {
			writeError(w, http.StatusConflict, "no_collecting_issue",
				"No round is collecting answers right now")
			return
		}

		if req.CurrentIssueID != current.ID {
			writeError(w, http.StatusConflict, "stale_schedule",
				"The current round changed since this page loaded — reload the dashboard and try again")
			return
		}

		day, pErr := time.ParseInLocation(scheduleDateLayout, req.CurrentCloseDate, loc)
		if pErr != nil {
			fieldErrs["current_close_date"] = "Invalid date, expected YYYY-MM-DD"
		} else {
			newCurrentClose = atLocalHour(day, deadlineLocalHour, loc)

			switch {
			case day.Before(today):
				fieldErrs["current_close_date"] = "The close date can't be in the past"
			case !newCurrentClose.After(now):
				// Today, but the 9 PM close instant already passed.
				fieldErrs["current_close_date"] = "Today's 9 PM close has already passed — pick a later date"
			case !newCurrentClose.After(current.OpensAt):
				fieldErrs["current_close_date"] = "The round can't close before it opened"
			case day.After(today.AddDate(0, 0, maxCloseDays)):
				fieldErrs["current_close_date"] = "Keep the close within 90 days"
			default:
				currentDeadline = newCurrentClose
			}
		}

		// A pinned next round this request isn't moving still bounds the
		// close: extended past the pinned open, the open event would fire
		// mid-collection, be consumed as a no-op, and silently unpin the
		// admin's schedule.
		if fieldErrs["current_close_date"] == "" && req.NextOpenDate == "" {
			pinned, pinErr := s.pinnedRoundOverlappingDeadline(
				ctx, groupID, newCurrentClose,
			)
			if pinErr != nil {
				s.logger.ErrorContext(ctx, "Failed to check pinned next round for schedule update",
					slog.String("error", pinErr.Error()))
				writeError(w, http.StatusInternalServerError, "server_error",
					"Failed to check the next round's schedule")

				return
			}

			if pinned != nil {
				fieldErrs["current_close_date"] = fmt.Sprintf(
					"The next round is pinned to open %s — close before then, or move both dates together",
					scheduleLabel(pinned.ScheduledAt, loc))
			}
		}
	}

	var nextOpen, nextClose time.Time

	windowDays := 0

	if req.NextOpenDate != "" {
		if !settings.AutoCreateEnabled {
			writeError(w, http.StatusConflict, "auto_create_disabled",
				"Auto-create is off — turn it on in Settings so scheduled rounds can open themselves")
			return
		}

		openDay, oErr := time.ParseInLocation(scheduleDateLayout, req.NextOpenDate, loc)
		closeDay, cErr := time.ParseInLocation(scheduleDateLayout, req.NextCloseDate, loc)

		switch {
		case oErr != nil:
			fieldErrs["next_open_date"] = "Invalid date, expected YYYY-MM-DD"
		case cErr != nil:
			fieldErrs["next_close_date"] = "Invalid date, expected YYYY-MM-DD"
		default:
			nextOpen = atLocalHour(openDay, nextOpenLocalHour, loc)
			nextClose = atLocalHour(closeDay, deadlineLocalHour, loc)
			// Calendar-day distance, DST-proof: re-parse the dates in UTC so
			// every day is exactly 24h long.
			openUTC, _ := time.Parse(scheduleDateLayout, req.NextOpenDate)
			closeUTC, _ := time.Parse(scheduleDateLayout, req.NextCloseDate)
			windowDays = int(closeUTC.Sub(openUTC) / (24 * time.Hour))

			switch {
			case openDay.Before(today.AddDate(0, 0, 1)):
				// Rounds open at 9 AM; most of the day that instant has
				// passed, so "opens today" would fire immediately —
				// "Start it now" is the path for that.
				fieldErrs["next_open_date"] = "The next round must open tomorrow or later"
			case openDay.After(today.AddDate(0, 0, maxNextOpenDays)):
				fieldErrs["next_open_date"] = "The next round must open within 6 months"
			case windowDays < minSubmissionWindowDays || windowDays > maxSubmissionWindowDays:
				fieldErrs["next_close_date"] = fmt.Sprintf(
					"Members need %d to %d days to answer",
					minSubmissionWindowDays, maxSubmissionWindowDays)
			case current != nil && !nextOpen.After(currentDeadline):
				fieldErrs["next_open_date"] = "The next round can't open before the current one closes"
			}
		}
	}

	if len(fieldErrs) > 0 {
		writeValidationError(w, fieldErrs)
		return
	}

	update := store.ScheduleUpdate{GroupID: groupID}

	if !newCurrentClose.IsZero() {
		update.CurrentIssueID = &current.ID
		update.CurrentDeadline = &newCurrentClose
		update.CurrentEvents = s.rescheduledIssueEventSpecs(
			ctx, settings, current.ID, current.Deadline, newCurrentClose,
		)
	}

	if !nextOpen.IsZero() {
		openLocal := nextOpen.In(loc)
		update.NextOpen = &nextOpen
		update.NextClose = &nextClose
		update.NextMonth = int(openLocal.Month())
		update.NextYear = openLocal.Year()
		update.WindowDays = windowDays
	}

	draftID, err := s.store.ApplySchedule(ctx, update)
	if err != nil {
		if errors.Is(err, store.ErrScheduleOverlap) {
			writeValidationError(w, map[string]string{
				"schedule": "The next round must open after the current round closes",
			})
			return
		}

		s.logger.ErrorContext(ctx, "Failed to apply schedule update",
			slog.Int64("group_id", groupID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to save the schedule")

		return
	}

	if !newCurrentClose.IsZero() {
		s.logger.InfoContext(ctx, "Issue deadline moved",
			slog.Int64("issue_id", current.ID),
			slog.Time("new_deadline", newCurrentClose))
	}

	if draftID != nil {
		settings.SubmissionWindowDays = windowDays
		s.logger.InfoContext(ctx, "Next round schedule pinned",
			slog.Int64("issue_id", *draftID),
			slog.Time("opens_at", nextOpen),
			slog.Time("deadline", nextClose),
			slog.Int("window_days", windowDays))
	}

	resp := map[string]any{}
	if !newCurrentClose.IsZero() {
		resp["current_deadline"] = newCurrentClose
	}

	if !nextOpen.IsZero() {
		resp["next_opens_at"] = nextOpen
		resp["next_deadline"] = nextClose
		resp["window_days"] = windowDays
	}

	writeJSON(w, http.StatusOK, resp)
}

// pinnedRoundOverlappingDeadline returns the queued next-round event when
// deadline would meet or pass its opening. Every API capable of moving a
// live deadline uses this guard, so older clients cannot silently consume a
// schedule pinned through the dashboard.
func (s *Server) pinnedRoundOverlappingDeadline(
	ctx context.Context, groupID int64, deadline time.Time,
) (*store.SchedulerEvent, error) {
	pinned, err := s.store.GetPendingNextRoundEvent(ctx, groupID)
	if err != nil || pinned == nil || pinned.ScheduledAt.After(deadline) {
		return nil, err
	}

	return pinned, nil
}

// NextRoundView is the dashboard's picture of the upcoming round for the
// schedule editor: human labels for display, bare loop-timezone dates to
// prefill the dialog's date inputs, and whether the schedule is actually
// queued (a pending create_next_issue event) or merely the cadence-derived
// default the next publish would produce.
type NextRoundView struct {
	OpensLabel  string
	ClosesLabel string
	OpenDate    string
	CloseDate   string
	Queued      bool
	// NeedsRepin marks a draft whose open event is missing (an earlier
	// queue failure, or a pin consumed by a no-op fire). The dialog must
	// send the next-round dates even when the admin leaves them untouched —
	// its diff-only save would otherwise close as if it saved and never
	// re-queue the round.
	NeedsRepin bool
}

// scheduleLabel formats an instant for schedule copy ("Fri, 15 Aug at 9:00 AM").
func scheduleLabel(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("Mon, 2 Jan at 3:04 PM")
}

// nextRoundView resolves what the schedule editor should show for the next
// round: the pinned schedule when one is queued, a stalled draft's schedule,
// or a cadence-derived suggestion anchored on the current round (matching
// what publish would queue). Returns nil when auto-create is off or there is
// nothing to anchor a suggestion on.
func (s *Server) nextRoundView(
	ctx context.Context, settings *store.Settings, current *store.Issue,
) *NextRoundView {
	if settings == nil || !settings.AutoCreateEnabled {
		return nil
	}

	loc := s.settingsLocation(ctx, settings)

	view := func(open, deadline time.Time, queued bool) *NextRoundView {
		return &NextRoundView{
			OpensLabel:  scheduleLabel(open, loc),
			ClosesLabel: scheduleLabel(deadline, loc),
			OpenDate:    open.In(loc).Format(scheduleDateLayout),
			CloseDate:   deadline.In(loc).Format(scheduleDateLayout),
			Queued:      queued,
		}
	}

	pending, err := s.store.GetPendingNextRoundEvent(ctx, settings.GroupID)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to look up queued next round",
			slog.String("error", err.Error()))

		return nil
	}

	if pending != nil {
		deadline := defaultDeadline(pending.ScheduledAt, settings, loc)

		if pending.IssueID != nil {
			if iss, iErr := s.store.GetIssueByID(ctx, *pending.IssueID); iErr == nil {
				deadline = iss.Deadline
			}
		}

		return view(pending.ScheduledAt, deadline, true)
	}

	// A draft without an event (earlier queue failure, or a pin consumed by
	// a no-op fire) still shows its schedule so the admin can re-pin it —
	// even when its open date has already slipped past. The current round
	// is excluded: a stalled past-open draft reads as active.
	if draft, dErr := s.store.GetNextDraftIssue(ctx, settings.GroupID); dErr == nil &&
		draft != nil && (current == nil || draft.ID != current.ID) {
		v := view(draft.OpensAt, draft.Deadline, false)
		v.NeedsRepin = true

		return v
	}

	if current == nil {
		return nil
	}

	open := cadenceNextOpen(current.OpensAt, settings.Frequency, loc)
	// An extended close can outrun the cadence slot, and a suggestion
	// inside the live round is unsaveable (the next round must open after
	// the close) — slide it to the morning after the close instead.
	if !open.After(current.Deadline) {
		open = atLocalHour(current.Deadline.In(loc).AddDate(0, 0, 1), nextOpenLocalHour, loc)
	}

	return view(open, defaultDeadline(open, settings, loc), false)
}

// issueLabel picks the (month, year) masthead label for a round opening in
// the base month: the open month, unless another issue in the group already
// claims it, in which case the store walks forward to the first free label
// (the archive addresses issues as /issues/{year}/{month}, so a duplicate
// would leave one edition permanently unreachable). One walk, shared with
// the schedule transaction, so the two can't drift. excludeIssueID is the
// issue being relabeled (0 when creating), whose own label doesn't count as
// taken. Lookup failures fall back to the base month.
func (s *Server) issueLabel(
	ctx context.Context, groupID, excludeIssueID int64, baseMonth, baseYear int,
) (month, year int) {
	month, year, err := s.store.NextAvailableIssueLabel(
		ctx, groupID, excludeIssueID, baseMonth, baseYear,
	)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to pick a free issue label — using the open month",
			slog.String("error", err.Error()))

		return baseMonth, baseYear
	}

	return month, year
}
