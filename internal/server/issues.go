package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/email"
	"github.com/parithosh/piecesoflife/internal/store"
)

// fallbackQuestionsPerIssue is the total question target used when settings
// are unreadable or hold a nonsensical value. The regular default lives in
// settings.questions_per_issue.
const fallbackQuestionsPerIssue = 6

// handleListIssues returns all issues, optionally filtered by status.
// GET /api/issues
func (s *Server) handleListIssues(w http.ResponseWriter, r *http.Request) {
	var statusFilter *string

	if v := r.URL.Query().Get("status"); v != "" {
		statusFilter = &v
	}

	issues, err := s.store.ListIssues(r.Context(), currentGroupID(r.Context()), statusFilter)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list issues",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list issues")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}

// handleGetIssue returns a single issue by ID, including its questions.
// GET /api/issues/{id}
func (s *Server) handleGetIssue(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	questions, err := s.store.ListQuestionsByIssue(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list questions for issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get questions")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"issue":     issue,
		"questions": questions,
	})
}

// createIssueRequest is the expected JSON body for POST /api/issues.
// QuestionCount is the total question target for this issue — default
// questions and member suggestions count toward it, and the bank only pads
// the shortfall. Zero or absent falls back to settings.questions_per_issue.
type createIssueRequest struct {
	Title         *string `json:"title"`
	Month         int     `json:"month"`
	Year          int     `json:"year"`
	OpensAt       string  `json:"opens_at"`
	Deadline      string  `json:"deadline"`
	QuestionCount int     `json:"question_count"`
}

// handleCreateIssue creates a new issue, auto-picks questions from the bank,
// and registers scheduler events.
// POST /api/issues
func (s *Server) handleCreateIssue(w http.ResponseWriter, r *http.Request) {
	var req createIssueRequest
	// Empty body is allowed — the dashboard "Create Issue" button posts no
	// payload and expects defaults derived from settings, matching the
	// scheduler's auto-create flow.
	if r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
			return
		}
	}

	groupID := currentGroupID(r.Context())

	settings, err := s.store.GetSettings(r.Context(), groupID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to load settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	now := time.Now().UTC()

	var opensAt, deadline time.Time

	if req.OpensAt != "" {
		opensAt, err = time.Parse(time.RFC3339, req.OpensAt)
		if err != nil {
			writeValidationError(w, map[string]string{
				"opens_at": "Invalid date format, expected RFC3339",
			})

			return
		}
	} else {
		opensAt = now
	}

	if req.Deadline != "" {
		deadline, err = time.Parse(time.RFC3339, req.Deadline)
		if err != nil {
			writeValidationError(w, map[string]string{
				"deadline": "Invalid date format, expected RFC3339",
			})

			return
		}
	} else {
		// Default deadline lands at a friendly evening hour in the loop's
		// configured timezone rather than a raw N×24h offset.
		deadline = defaultDeadline(
			opensAt, settings, s.settingsLocation(r.Context(), settings),
		)
	}

	// A blank title must be stored as NULL, not "" — templates fall back to
	// the issue month via {{if .Issue.Title}}, and a non-nil empty string
	// renders a blank masthead instead.
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		req.Title = nil
	}

	if req.Month == 0 {
		req.Month = int(opensAt.Month())
	}

	if req.Year == 0 {
		req.Year = opensAt.Year()
	}

	validationErrs := make(map[string]string, 3)

	if req.Month < 1 || req.Month > 12 {
		validationErrs["month"] = "Month must be between 1 and 12"
	}

	if req.Year < 2020 {
		validationErrs["year"] = "Year must be 2020 or later"
	}

	if req.QuestionCount < 0 || req.QuestionCount > 20 {
		validationErrs["question_count"] = "Must be between 1 and 20 questions"
	}

	if len(validationErrs) > 0 {
		writeValidationError(w, validationErrs)
		return
	}

	if !deadline.After(opensAt) {
		writeValidationError(w, map[string]string{
			"deadline": "Deadline must be after opens_at",
		})

		return
	}

	hasActive, err := s.store.HasActiveIssue(r.Context(), groupID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check for active issue",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	if hasActive {
		writeError(w, http.StatusConflict, "conflict",
			"An active issue already exists. Publish or close it before creating a new one.")
		return
	}

	// If the next round already exists as a pre-created draft, "create"
	// means "start it now": pull its schedule forward and open it, keeping
	// any question suggestions members already sent it.
	if draft, dErr := s.store.GetUpcomingDraftIssue(r.Context(), groupID); dErr != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check for upcoming draft",
			slog.String("error", dErr.Error()))
	} else if draft != nil {
		loc := s.settingsLocation(r.Context(), settings)
		nowLocal := now.In(loc)
		newDeadline := defaultDeadline(now, settings, loc)
		// Relabeling to the open month must not duplicate a (month, year)
		// the archive already uses — /issues/{year}/{month} can only ever
		// resolve one edition per label.
		month, year := s.issueLabel(r.Context(), groupID, draft.ID,
			int(nowLocal.Month()), nowLocal.Year())

		fresh := *draft
		fresh.Month = month
		fresh.Year = year
		fresh.OpensAt = now
		fresh.Deadline = newDeadline
		if req.Title != nil {
			fresh.Title = req.Title
		}
		lifecycleEvents, _ := s.issueEventSpecs(
			r.Context(), settings, draft.ID, newDeadline,
		)

		transition := func() error {
			return s.store.OpenDraftEarly(
				r.Context(), groupID, draft.ID, req.Title != nil, req.Title,
				month, year, now, newDeadline, lifecycleEvents,
			)
		}

		if oErr := s.openIssueForCollectingWithTransition(
			r.Context(), settings, &fresh, req.QuestionCount, transition, true,
		); oErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to open upcoming draft",
				slog.Int64("issue_id", draft.ID),
				slog.String("error", oErr.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to open the upcoming issue")

			return
		}

		opened, oErr := s.store.GetIssueByID(r.Context(), draft.ID)
		if oErr != nil {
			opened = &fresh
		}

		s.logger.InfoContext(r.Context(), "Opened pre-created draft early",
			slog.Int64("issue_id", draft.ID))
		writeJSON(w, http.StatusCreated, map[string]any{"issue": opened})

		return
	}

	// The fresh round claims the first free (month, year) at or after the
	// requested one — every sibling creation path dedupes the same way, and
	// a duplicate label would make one archive edition unreachable.
	month, year := s.issueLabel(r.Context(), groupID, 0, req.Month, req.Year)

	issueID, err := s.store.CreateIssue(
		r.Context(), groupID, req.Title, month, year, opensAt, deadline,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create issue",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create issue")

		return
	}

	if err := s.store.SetIssueStatus(r.Context(), issueID, "collecting"); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to set issue status",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	}

	// Default questions lead the round and count toward the total target;
	// the bank only pads the shortfall.
	numDefaults := s.insertDefaultQuestions(r.Context(), groupID, issueID, 0)

	questionCount := questionTarget(settings, req.QuestionCount) - numDefaults

	var bankQuestions []store.QuestionBank
	if questionCount > 0 {
		bankQuestions, err = s.store.SelectRandomUnusedQuestions(r.Context(), groupID, questionCount)
		if err != nil {
			s.logger.ErrorContext(r.Context(), "Failed to select bank questions",
				slog.Int64("issue_id", issueID),
				slog.String("error", err.Error()))
		}
	}

	for i, bq := range bankQuestions {
		cat := bq.Category

		if _, qErr := s.store.CreateQuestion(
			r.Context(), issueID, bq.Text, &cat, "bank", nil, numDefaults+i,
		); qErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to create question from bank",
				slog.Int64("bank_question_id", bq.ID),
				slog.String("error", qErr.Error()))

			continue
		}

		if markErr := s.store.MarkBankQuestionUsed(r.Context(), bq.ID); markErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to mark bank question as used",
				slog.Int64("bank_question_id", bq.ID),
				slog.String("error", markErr.Error()))
		}
	}

	// Queue reminders and auto-close anchored to the deadline.
	if _, evErr := s.scheduleIssueEvents(r.Context(), settings, issueID, deadline); evErr != nil {
		s.logger.ErrorContext(r.Context(), "Failed to schedule issue events — round will not auto-close; extend the deadline to requeue",
			slog.Int64("issue_id", issueID),
			slog.String("error", evErr.Error()))
	}

	s.logger.InfoContext(r.Context(), "Issue created",
		slog.Int64("issue_id", issueID),
		slog.Int("questions_added", len(bankQuestions)),
	)

	issue, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch created issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusCreated, map[string]any{"id": issueID})

		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"issue": issue})
}

// updateIssueRequest is the expected JSON body for PATCH /api/issues/{id}.
type updateIssueRequest struct {
	Title    *string `json:"title"`
	Deadline *string `json:"deadline"`
}

// handleUpdateIssue updates an issue's title and/or deadline.
// PATCH /api/issues/{id}
func (s *Server) handleUpdateIssue(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	var req updateIssueRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	var deadlineTime *time.Time

	if req.Deadline != nil {
		t, parseErr := time.Parse(time.RFC3339, *req.Deadline)
		if parseErr != nil {
			writeValidationError(w, map[string]string{
				"deadline": "Invalid date format, expected RFC3339",
			})

			return
		}

		deadlineTime = &t
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	if deadlineTime != nil {
		if issue.Status != "collecting" && issue.Status != "draft" {
			writeError(w, http.StatusConflict, "not_collecting",
				"Only a collecting or upcoming round's deadline can be changed")
			return
		}

		if !deadlineTime.After(time.Now()) || !deadlineTime.After(issue.OpensAt) {
			writeValidationError(w, map[string]string{
				"deadline": "Deadline must be in the future and after the round opened",
			})
			return
		}
	}

	switch {
	case deadlineTime != nil && issue.Status == "collecting":
		if !s.deadlineClearsPinnedRound(
			w, r, issue.GroupID, *deadlineTime, "deadline",
		) {
			return
		}

		// Title and deadline commit together — a reschedule moves every
		// member's reminder emails, so it must not land while the response
		// reports failure on the other half.
		if err := s.rescheduleIssueDeadline(
			r.Context(), issue, *deadlineTime, req.Title,
		); err != nil {
			if errors.Is(err, store.ErrScheduleOverlap) {
				writeValidationError(w, map[string]string{
					"deadline": "The current round must close before the queued next round opens",
				})
				return
			}

			s.logger.ErrorContext(r.Context(), "Failed to reschedule issue",
				slog.Int64("issue_id", issueID),
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to update issue")

			return
		}
	case deadlineTime != nil:
		// An upcoming draft has no reminder or auto-close events yet — its
		// deadline (and title) update is one plain statement. The queued
		// open event tracks the open time, not the close, so no re-pin is
		// needed.
		if err := s.store.UpdateIssue(r.Context(), issueID, req.Title, deadlineTime); err != nil {
			s.logger.ErrorContext(r.Context(), "Failed to update issue",
				slog.Int64("issue_id", issueID),
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to update issue")

			return
		}
	case req.Title != nil:
		if err := s.store.UpdateIssue(r.Context(), issueID, req.Title, nil); err != nil {
			s.logger.ErrorContext(r.Context(), "Failed to update issue",
				slog.Int64("issue_id", issueID),
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to update issue")

			return
		}
	}

	issue, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch updated issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"issue": issue})
}

// handlePublishIssue publishes an issue and sends notification emails in the background.
// POST /api/issues/{id}/publish
func (s *Server) handlePublishIssue(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	if issue.Status == "published" {
		writeJSON(w, http.StatusOK, map[string]any{
			"issue":    issue,
			"redirect": issuePublicPath(issue),
		})
		return
	}

	if err := s.store.PublishIssue(r.Context(), issueID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to publish issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to publish issue")

		return
	}

	// Gather submitted responses to include in the publish summary.
	responses, err := s.store.ListResponsesByIssue(r.Context(), issueID, true)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list responses for publish",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	}

	// Gather active members for notification.
	members, err := s.store.ListActiveGroupMembers(r.Context(), issue.GroupID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list active members for publish notification",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	}

	settings, err := s.store.GetSettings(r.Context(), issue.GroupID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to get settings for publish notification",
			slog.String("error", err.Error()))
	}

	s.logger.InfoContext(r.Context(), "Issue published",
		slog.Int64("issue_id", issueID),
		slog.Int("submitted_responses", len(responses)),
	)

	// Refresh issue to include the updated published_at timestamp.
	updatedIssue, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		updatedIssue = issue
	}

	// Manual publish schedules the next round exactly like auto-publish
	// does — otherwise the loop silently stalls on "no active issue".
	if settings != nil && settings.AutoCreateEnabled {
		s.queueNextIssueEvent(r.Context(), settings, updatedIssue.OpensAt)
	}

	// Detach the context so the publish batch survives the HTTP request.
	go s.sendPublishNotifications(context.WithoutCancel(r.Context()), updatedIssue, members, settings)

	writeJSON(w, http.StatusOK, map[string]any{
		"issue":                    updatedIssue,
		"redirect":                 issuePublicPath(updatedIssue),
		"submitted_response_count": len(responses),
	})
}

func issuePublicPath(issue *store.Issue) string {
	if issue == nil {
		return "/issues"
	}

	return fmt.Sprintf("/issues/%d/%02d", issue.Year, issue.Month)
}

// extendDeadlineRequest is the expected JSON body for POST /api/issues/{id}/extend.
type extendDeadlineRequest struct {
	NewDeadline string `json:"new_deadline"`
}

// handleExtendDeadline extends the deadline of an issue and recreates scheduler events.
// POST /api/issues/{id}/extend
func (s *Server) handleExtendDeadline(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	var req extendDeadlineRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if req.NewDeadline == "" {
		writeValidationError(w, map[string]string{
			"new_deadline": "new_deadline is required",
		})

		return
	}

	newDeadline, err := time.Parse(time.RFC3339, req.NewDeadline)
	if err != nil {
		writeValidationError(w, map[string]string{
			"new_deadline": "Invalid date format, expected RFC3339",
		})

		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	if issue.Status != "collecting" {
		writeError(w, http.StatusConflict, "not_collecting",
			"Cannot extend a round that is not collecting answers")
		return
	}

	if !newDeadline.After(time.Now()) || !newDeadline.After(issue.Deadline) {
		writeValidationError(w, map[string]string{
			"new_deadline": "New deadline must be in the future and later than the current deadline",
		})

		return
	}

	if !s.deadlineClearsPinnedRound(
		w, r, issue.GroupID, newDeadline, "new_deadline",
	) {
		return
	}

	if err := s.rescheduleIssueDeadline(r.Context(), issue, newDeadline, nil); err != nil {
		if errors.Is(err, store.ErrScheduleOverlap) {
			writeValidationError(w, map[string]string{
				"new_deadline": "The current round must close before the queued next round opens",
			})
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to update issue deadline",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to extend deadline")

		return
	}

	s.logger.InfoContext(r.Context(), "Issue deadline extended",
		slog.Int64("issue_id", issueID),
		slog.Time("new_deadline", newDeadline),
	)

	updated, err := s.store.GetIssueByID(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch updated issue after extend",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"issue": updated})
}

// deadlineClearsPinnedRound applies the shared invariant for every legacy
// deadline API: a collecting round must close before its queued successor
// opens. Store failures are reported as server errors, not as "no pin".
func (s *Server) deadlineClearsPinnedRound(
	w http.ResponseWriter, r *http.Request, groupID int64,
	deadline time.Time, field string,
) bool {
	pinned, err := s.pinnedRoundOverlappingDeadline(r.Context(), groupID, deadline)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check pinned next round",
			slog.Int64("group_id", groupID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to check the next round's schedule")

		return false
	}

	if pinned != nil {
		writeValidationError(w, map[string]string{
			field: "The current round must close before the queued next round opens",
		})

		return false
	}

	return true
}

// rescheduledIssueEventSpecs rebuilds reminders and auto-close around a new
// deadline. If no standard member reminder has enough lead time, it includes
// one immediate last-chance reminder — always when the deadline moved
// earlier (whatever close the last reminder advertised is now wrong, and the
// round would close under members who think they have more time), and
// otherwise unless a reminder already fired in the last day (successive
// extensions must not spam non-responders).
func (s *Server) rescheduledIssueEventSpecs(
	ctx context.Context, settings *store.Settings, issueID int64,
	oldDeadline, newDeadline time.Time,
) []store.SchedulerEventSpec {
	events, remindersQueued := s.issueEventSpecs(ctx, settings, issueID, newDeadline)
	if remindersQueued != 0 {
		return events
	}

	immediate := store.SchedulerEventSpec{
		EventType: "reminder_2", ScheduledAt: time.Now(),
	}

	if newDeadline.Before(oldDeadline) {
		return append(events, immediate)
	}

	recent, err := s.store.HasRecentFiredReminder(ctx, issueID)
	switch {
	case err != nil:
		s.logger.ErrorContext(ctx, "Failed to check recent reminders — skipping immediate last-chance reminder",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	case recent:
		s.logger.InfoContext(ctx, "Skipping immediate reminder — one already went out in the last day",
			slog.Int64("issue_id", issueID))
	default:
		events = append(events, immediate)
	}

	return events
}

// rescheduleIssueDeadline atomically moves a collecting round's deadline and
// replaces all of its pending lifecycle events; a non-nil title renames the
// round in the same transaction.
func (s *Server) rescheduleIssueDeadline(
	ctx context.Context, issue *store.Issue, newDeadline time.Time, title *string,
) error {
	settings, settingsErr := s.store.GetSettings(ctx, issue.GroupID)
	if settingsErr != nil {
		s.logger.WarnContext(ctx, "Failed to load settings for reschedule — using UTC",
			slog.String("error", settingsErr.Error()))
	}

	events := s.rescheduledIssueEventSpecs(
		ctx, settings, issue.ID, issue.Deadline, newDeadline,
	)
	_, err := s.store.ApplySchedule(ctx, store.ScheduleUpdate{
		GroupID:         issue.GroupID,
		CurrentIssueID:  &issue.ID,
		CurrentDeadline: &newDeadline,
		CurrentTitle:    title,
		CurrentEvents:   events,
	})
	if err != nil {
		return fmt.Errorf("rescheduling issue %d: %w", issue.ID, err)
	}

	s.logger.InfoContext(ctx, "Issue deadline moved",
		slog.Int64("issue_id", issue.ID),
		slog.Time("new_deadline", newDeadline))

	return nil
}

// handleListQuestions returns all questions for an issue.
// GET /api/issues/{id}/questions
func (s *Server) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	questions, err := s.store.ListQuestionsByIssue(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list questions",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list questions")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"questions": questions})
}

// handleGetProgress returns submission progress for an issue.
// GET /api/issues/{id}/progress
func (s *Server) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	progress, err := s.store.GetSubmissionProgress(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to get submission progress",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get progress")

		return
	}

	writeJSON(w, http.StatusOK, progress)
}

// handleSetCountAdmin toggles whether admins are counted in an issue's
// progress denominator, then returns the refreshed progress.
// POST /api/issues/{id}/count-admin
func (s *Server) handleSetCountAdmin(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	var body struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid request body")
		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	if err := s.store.SetIssueCountAdminIn(r.Context(), issueID, body.On); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to set count_admin_in",
			slog.Int64("issue_id", issueID),
			slog.Bool("on", body.On),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update setting")

		return
	}

	progress, err := s.store.GetSubmissionProgress(r.Context(), issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to get submission progress",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get progress")

		return
	}

	writeJSON(w, http.StatusOK, progress)
}

// handleListResponses returns submitted responses for an issue, enriched with blocks.
// GET /api/issues/{id}/responses
func (s *Server) handleListResponses(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	responses, err := s.store.ListResponsesByIssue(r.Context(), issueID, true)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list responses",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list responses")

		return
	}

	enriched, err := s.enrichResponsesWithBlocks(r.Context(), responses)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to enrich responses with blocks",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load responses")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"responses": enriched})
}

// handleListMyResponses returns the current user's responses for an issue.
// GET /api/issues/{id}/responses/mine
func (s *Server) handleListMyResponses(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	user := UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	responses, err := s.store.ListUserResponsesForIssue(r.Context(), user.ID, issueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list user responses",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list responses")

		return
	}

	enriched, err := s.enrichResponsesWithBlocks(r.Context(), responses)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to enrich user responses with blocks",
			slog.Int64("issue_id", issueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to load responses")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"responses": enriched})
}

// addQuestionRequest is the expected JSON body for POST /api/issues/{id}/questions.
type addQuestionRequest struct {
	Text      string  `json:"text"`
	Category  *string `json:"category"`
	Source    string  `json:"source"`
	SortOrder int     `json:"sort_order"`
}

// handleAddQuestion adds a question to an issue.
// POST /api/issues/{id}/questions
func (s *Server) handleAddQuestion(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	var req addQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if strings.TrimSpace(req.Text) == "" {
		writeValidationError(w, map[string]string{
			"text": "Question text is required",
		})

		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	source := req.Source
	if source == "" {
		source = "admin"
	}
	if source != "admin" && source != "bank" && source != "friend" {
		writeValidationError(w, map[string]string{
			"source": "Source must be admin, bank, or friend",
		})
		return
	}

	// No explicit sort_order appends after the existing questions — a zero
	// default would silently jump the new question to the front.
	sortOrder := req.SortOrder
	if sortOrder <= 0 {
		if existing, lErr := s.store.ListQuestionsByIssue(r.Context(), issueID); lErr == nil {
			sortOrder = len(existing)
		}
	}

	questionID, err := s.store.CreateQuestion(
		r.Context(), issueID, req.Text, req.Category, source, nil, sortOrder,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create question",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to add question")

		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), questionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch created question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusCreated, map[string]any{"id": questionID})

		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"question": question})
}

// editQuestionRequest is the expected JSON body for PATCH /api/questions/{id}.
type editQuestionRequest struct {
	Text string `json:"text"`
}

// handleEditQuestion updates a question's text.
// PATCH /api/questions/{id}
func (s *Server) handleEditQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	var req editQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if strings.TrimSpace(req.Text) == "" {
		writeValidationError(w, map[string]string{
			"text": "Question text is required",
		})

		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), questionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Question not found")
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to fetch question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	if _, ok := s.requireIssue(w, r, question.IssueID); !ok {
		return
	}

	if err := s.store.UpdateQuestion(r.Context(), questionID, req.Text); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update question")

		return
	}

	question, err = s.store.GetQuestionByID(r.Context(), questionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch updated question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"question": question})
}

// reorderQuestionsRequest is the expected JSON body for
// POST /api/issues/{id}/questions/reorder.
type reorderQuestionsRequest struct {
	QuestionIDs []int64 `json:"question_ids"`
}

// handleReorderQuestions saves a new order for an issue's questions. The
// list must cover the issue's questions exactly.
// POST /api/issues/{id}/questions/reorder
func (s *Server) handleReorderQuestions(w http.ResponseWriter, r *http.Request) {
	issueID, ok := s.parseIDParam(w, r, "id", "issue ID")
	if !ok {
		return
	}

	var req reorderQuestionsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	if len(req.QuestionIDs) == 0 {
		writeValidationError(w, map[string]string{
			"question_ids": "question_ids is required",
		})

		return
	}

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	if err := s.store.ReorderQuestions(r.Context(), issueID, req.QuestionIDs); err != nil {
		if errors.Is(err, store.ErrOrderMismatch) {
			writeError(w, http.StatusConflict, "stale_order",
				"The question list changed — reload and try again")
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to reorder questions",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error",
			"Failed to reorder questions")

		return
	}

	questions, err := s.store.ListQuestionsByIssue(r.Context(), issueID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"questions": questions})
}

// handleDeleteQuestion removes a question by ID.
// DELETE /api/questions/{id}
func (s *Server) handleDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), questionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Question not found")
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to fetch question for delete",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")

		return
	}

	if _, ok := s.requireIssue(w, r, question.IssueID); !ok {
		return
	}

	if err := s.store.DeleteQuestion(r.Context(), questionID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete question")

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// friendSubmitQuestionRequest is the expected JSON body for POST /api/questions/submit.
type friendSubmitQuestionRequest struct {
	IssueID  int64   `json:"issue_id"`
	Text     string  `json:"text"`
	Category *string `json:"category"`
}

// handleFriendSubmitQuestion allows a member to submit a custom question for an issue.
// POST /api/questions/submit
func (s *Server) handleFriendSubmitQuestion(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	var req friendSubmitQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	validationErrs := make(map[string]string, 2)

	if req.IssueID <= 0 {
		validationErrs["issue_id"] = "Valid issue_id is required"
	}

	if strings.TrimSpace(req.Text) == "" {
		validationErrs["text"] = "Question text is required"
	}

	if len(validationErrs) > 0 {
		writeValidationError(w, validationErrs)
		return
	}

	issue, ok := s.requireIssue(w, r, req.IssueID)
	if !ok {
		return
	}

	// Member suggestions only land on the NEXT round — the upcoming draft
	// pre-created at publish time. The current collecting round is curated
	// by the admin alone (dashboard question editor).
	acceptsQuestions := issue.Status == "draft" && issue.OpensAt.After(time.Now())
	if !acceptsQuestions {
		writeError(w, http.StatusConflict, "not_accepting_suggestions",
			"Suggestions are only accepted for the next issue, before it opens")
		return
	}

	// Determine sort order by appending after existing questions.
	existingQuestions, err := s.store.ListQuestionsByIssue(r.Context(), req.IssueID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list questions for sort order",
			slog.Int64("issue_id", req.IssueID),
			slog.String("error", err.Error()))
	}

	sortOrder := len(existingQuestions)

	questionID, err := s.store.CreateQuestion(
		r.Context(), req.IssueID, req.Text, req.Category, "friend", &user.ID, sortOrder,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create friend question",
			slog.Int64("issue_id", req.IssueID),
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to submit question")

		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), questionID)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to fetch submitted question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeJSON(w, http.StatusCreated, map[string]any{"id": questionID})

		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"question": question})
}

// handleListQuestionBank returns paginated question bank entries.
// GET /api/question-bank
func (s *Server) handleListQuestionBank(w http.ResponseWriter, r *http.Request) {
	pg := parsePagination(r)

	var category *string

	if v := r.URL.Query().Get("category"); v != "" {
		category = &v
	}

	var used *bool

	if v := r.URL.Query().Get("used"); v != "" {
		b := v == "true" || v == "1"
		used = &b
	}

	questions, total, err := s.store.ListQuestionBank(
		r.Context(), currentGroupID(r.Context()), category, used, pg.Page, pg.PerPage,
	)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list question bank",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list question bank")

		return
	}

	writeJSON(w, http.StatusOK, ListResponse[store.QuestionBank]{
		Items:   questions,
		Total:   total,
		Page:    pg.Page,
		PerPage: pg.PerPage,
	})
}

// createBankQuestionRequest is the expected JSON body for POST /api/question-bank.
type createBankQuestionRequest struct {
	Text     string `json:"text"`
	Category string `json:"category"`
}

// handleCreateBankQuestion creates a new question in the bank.
// POST /api/question-bank
func (s *Server) handleCreateBankQuestion(w http.ResponseWriter, r *http.Request) {
	var req createBankQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	validationErrs := make(map[string]string, 2)

	if strings.TrimSpace(req.Text) == "" {
		validationErrs["text"] = "Question text is required"
	}

	if strings.TrimSpace(req.Category) == "" {
		validationErrs["category"] = "Category is required"
	}

	if len(validationErrs) > 0 {
		writeValidationError(w, validationErrs)
		return
	}

	questionID, err := s.store.CreateBankQuestion(
		r.Context(), currentGroupID(r.Context()), req.Text, req.Category)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create bank question",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create bank question")

		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":       questionID,
		"text":     req.Text,
		"category": req.Category,
	})
}

// editBankQuestionRequest is the expected JSON body for PATCH /api/question-bank/{id}.
type editBankQuestionRequest struct {
	Text     string `json:"text"`
	Category string `json:"category"`
}

// handleEditBankQuestion updates a bank question's text and category.
// PATCH /api/question-bank/{id}
func (s *Server) handleEditBankQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	var req editBankQuestionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	validationErrs := make(map[string]string, 2)

	if strings.TrimSpace(req.Text) == "" {
		validationErrs["text"] = "Question text is required"
	}

	if strings.TrimSpace(req.Category) == "" {
		validationErrs["category"] = "Category is required"
	}

	if len(validationErrs) > 0 {
		writeValidationError(w, validationErrs)
		return
	}

	if err := s.store.UpdateBankQuestion(r.Context(),
		currentGroupID(r.Context()), questionID, req.Text, req.Category); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update bank question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update bank question")

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":       questionID,
		"text":     req.Text,
		"category": req.Category,
	})
}

// handleDeleteBankQuestion removes a bank question by ID.
// DELETE /api/question-bank/{id}
func (s *Server) handleDeleteBankQuestion(w http.ResponseWriter, r *http.Request) {
	questionID, ok := s.parseIDParam(w, r, "id", "question ID")
	if !ok {
		return
	}

	if err := s.store.DeleteBankQuestion(r.Context(),
		currentGroupID(r.Context()), questionID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete bank question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to delete bank question")

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// enrichResponsesWithBlocks loads content blocks and author for each response.
func (s *Server) enrichResponsesWithBlocks(
	ctx context.Context, responses []store.Response,
) ([]store.ResponseWithBlocks, error) {
	result := make([]store.ResponseWithBlocks, 0, len(responses))

	for _, resp := range responses {
		blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
		if err != nil {
			return nil, fmt.Errorf("listing blocks for response %d: %w", resp.ID, err)
		}

		author, err := s.store.GetUserByID(ctx, resp.UserID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to fetch user for response",
				slog.Int64("response_id", resp.ID),
				slog.Int64("user_id", resp.UserID),
				slog.String("error", err.Error()))

			// Fall back to a stub user so callers still receive all responses.
			author = &store.User{ID: resp.UserID}
		}

		result = append(result, store.ResponseWithBlocks{
			Response: resp,
			Blocks:   blocks,
			User:     *author,
		})
	}

	return result, nil
}

// sendPublishNotifications sends "issue published" emails to all active users who
// have the published notification preference enabled. Intended to run in a goroutine.
func (s *Server) sendPublishNotifications(
	ctx context.Context,
	issue *store.Issue,
	members []store.GroupMember,
	settings *store.Settings,
) {
	if settings == nil {
		s.logger.ErrorContext(ctx, "Cannot send publish notifications: settings unavailable",
			slog.Int64("issue_id", issue.ID))
		return
	}

	monthStr := formatDate(issue.OpensAt)
	subject := fmt.Sprintf("%s — %s is published!", settings.LoopName, monthStr)

	// The year/month reading URL is ambiguous across Loops, so it always
	// carries ?g= — with the per-user auth token when minting succeeds.
	issueURL := fmt.Sprintf("%s/issues/%d/%02d?g=%d",
		s.config.BaseURL, issue.Year, issue.Month, issue.GroupID)

	recipients := make([]email.BatchRecipient, 0, len(members))

	for i := range members {
		u := &members[i]

		prefs, err := s.store.GetNotificationPreferences(ctx, u.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.logger.ErrorContext(ctx, "Failed to get notification preferences for user",
				slog.Int64("user_id", u.ID),
				slog.String("error", err.Error()))

			continue
		}

		if prefs != nil && !prefs.Published {
			continue
		}

		// Per-recipient magic link straight to the reading view — no
		// login dance. If minting fails, fall back to the plain URL; the
		// login page still gets them there.
		readURL := issueURL

		if raw, tokenErr := s.mintEmailCTAToken(ctx, u.ID); tokenErr == nil {
			readURL = issueURL + "&auth=" + raw
		} else {
			s.logger.ErrorContext(ctx, "Failed to mint publish notification token",
				slog.Int64("user_id", u.ID),
				slog.String("error", tokenErr.Error()))
		}

		recipients = append(recipients, email.BatchRecipient{
			UserID:    u.ID,
			Email:     u.Email,
			Subject:   subject,
			HTMLBody:  s.renderPublishEmail(settings.LoopName, u.Name, monthStr, readURL),
			IssueID:   &issue.ID,
			EmailType: "published",
		})
	}

	s.emailer.SendBatch(ctx, recipients, func(batchCtx context.Context, rec email.BatchRecipient, sendErr error) {
		if sendErr != nil {
			errStr := sendErr.Error()
			_, _ = s.store.LogEmail(batchCtx, &issue.GroupID, &rec.UserID, rec.IssueID,
				rec.EmailType, "failed", &errStr)
			s.logger.ErrorContext(batchCtx, "Failed to send publish notification",
				slog.Int64("user_id", rec.UserID),
				slog.String("error", errStr))

			return
		}

		_, _ = s.store.LogEmail(batchCtx, &issue.GroupID, &rec.UserID, rec.IssueID,
			rec.EmailType, "sent", nil)
	})
}

// insertDefaultQuestions adds every enabled default question to an issue
// with source "default", starting at sortOrder, and returns how many were
// added. Failures are logged per question — a missing default prompt must
// not block a round from opening.
func (s *Server) insertDefaultQuestions(
	ctx context.Context, groupID, issueID int64, sortOrder int,
) int {
	defaults, err := s.store.ListEnabledDefaultQuestions(ctx, groupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list default questions",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))

		return 0
	}

	added := 0

	for _, dq := range defaults {
		if _, err := s.store.CreateQuestion(
			ctx, issueID, dq.Text, nil, "default", nil, sortOrder+added,
		); err != nil {
			s.logger.ErrorContext(ctx, "Failed to add default question to issue",
				slog.Int64("issue_id", issueID),
				slog.Int64("default_question_id", dq.ID),
				slog.String("error", err.Error()))

			continue
		}

		added++
	}

	return added
}

// questionTarget resolves the total number of questions an issue aims for:
// an explicit per-issue choice wins, then the settings default, then the
// hardcoded fallback. Default questions and member suggestions count toward
// it — the bank only pads up to this number, and suggestions beyond it are
// welcome.
func questionTarget(settings *store.Settings, requested int) int {
	if requested > 0 {
		return requested
	}

	if settings != nil && settings.QuestionsPerIssue > 0 {
		return settings.QuestionsPerIssue
	}

	return fallbackQuestionsPerIssue
}

// renderPublishEmail builds the "issue published" notification via the
// published.html template. html/template auto-escapes every field.
func (s *Server) renderPublishEmail(
	loopName, recipientName, month, readURL string,
) string {
	return s.renderEmail("published.html", map[string]any{
		"LoopName":      loopName,
		"RecipientName": recipientName,
		"Month":         month,
		"CTA": map[string]any{
			"URL":   readURL,
			"Label": "Read the Issue",
		},
	})
}
