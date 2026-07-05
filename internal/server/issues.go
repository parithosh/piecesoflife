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

	issues, err := s.store.ListIssues(r.Context(), statusFilter)
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

	settings, err := s.store.GetSettings(r.Context())
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
		windowDays := effectiveWindowDays(settings)

		// Default deadline lands at a friendly evening hour in the loop's
		// configured timezone rather than a raw N×24h offset.
		loc := s.settingsLocation(r.Context(), settings)
		deadline = atLocalHour(
			opensAt.In(loc).AddDate(0, 0, windowDays), deadlineLocalHour, loc,
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

	hasActive, err := s.store.HasActiveIssue(r.Context())
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
	if draft, dErr := s.store.GetUpcomingDraftIssue(r.Context()); dErr != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check for upcoming draft",
			slog.String("error", dErr.Error()))
	} else if draft != nil {
		if req.Title != nil {
			if uErr := s.store.UpdateIssue(r.Context(), draft.ID, req.Title, nil); uErr != nil {
				s.logger.WarnContext(r.Context(), "Failed to title early-opened draft",
					slog.Int64("issue_id", draft.ID),
					slog.String("error", uErr.Error()))
			}
		}

		loc := s.settingsLocation(r.Context(), settings)
		windowDays := effectiveWindowDays(settings)
		newDeadline := atLocalHour(now.In(loc).AddDate(0, 0, windowDays), deadlineLocalHour, loc)

		if uErr := s.store.UpdateIssueSchedule(r.Context(), draft.ID, now, newDeadline); uErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to reschedule early-opened draft",
				slog.Int64("issue_id", draft.ID),
				slog.String("error", uErr.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to open the upcoming issue")

			return
		}

		fresh, fErr := s.store.GetIssueByID(r.Context(), draft.ID)
		if fErr != nil {
			fresh = draft
		}

		if oErr := s.openIssueForCollecting(r.Context(), settings, fresh, req.QuestionCount); oErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to open upcoming draft",
				slog.Int64("issue_id", draft.ID),
				slog.String("error", oErr.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to open the upcoming issue")

			return
		}

		opened, oErr := s.store.GetIssueByID(r.Context(), draft.ID)
		if oErr != nil {
			opened = fresh
		}

		s.logger.InfoContext(r.Context(), "Opened pre-created draft early",
			slog.Int64("issue_id", draft.ID))
		writeJSON(w, http.StatusCreated, map[string]any{"issue": opened})

		return
	}

	issueID, err := s.store.CreateIssue(
		r.Context(), req.Title, req.Month, req.Year, opensAt, deadline,
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
	numDefaults := s.insertDefaultQuestions(r.Context(), issueID, 0)

	questionCount := questionTarget(settings, req.QuestionCount) - numDefaults

	var bankQuestions []store.QuestionBank
	if questionCount > 0 {
		bankQuestions, err = s.store.SelectRandomUnusedQuestions(r.Context(), questionCount)
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

	// Create scheduler events based on the deadline window. Reminders are
	// pinned to a mid-morning hour in the loop's timezone.
	evLoc := s.settingsLocation(r.Context(), settings)
	windowDuration := deadline.Sub(opensAt)
	reminder1At := atLocalHour(opensAt.Add(windowDuration/2), reminderLocalHour, evLoc)
	reminder2At := atLocalHour(deadline.AddDate(0, 0, -2), reminderLocalHour, evLoc)

	for _, ev := range []struct {
		eventType   string
		scheduledAt time.Time
	}{
		{"reminder_1", reminder1At},
		{"reminder_2", reminder2At},
		{"auto_close", deadline},
	} {
		if evErr := s.store.CreateSchedulerEvent(
			r.Context(), &issueID, ev.eventType, ev.scheduledAt,
		); evErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to create scheduler event",
				slog.String("event_type", ev.eventType),
				slog.String("error", evErr.Error()))
		}
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

	if _, ok := s.requireIssue(w, r, issueID); !ok {
		return
	}

	if err := s.store.UpdateIssue(r.Context(), issueID, req.Title, deadlineTime); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update issue",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update issue")

		return
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

	// Gather active users for notification.
	users, err := s.store.ListActiveUsers(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list active users for publish notification",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	}

	settings, err := s.store.GetSettings(r.Context())
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
	go s.sendPublishNotifications(context.WithoutCancel(r.Context()), updatedIssue, users, settings)

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

	if issue.Status == "published" {
		writeError(w, http.StatusConflict, "already_published",
			"Cannot extend deadline of a published issue")
		return
	}

	if !newDeadline.After(issue.Deadline) {
		writeValidationError(w, map[string]string{
			"new_deadline": "New deadline must be later than the current deadline",
		})

		return
	}

	if err := s.store.UpdateIssue(r.Context(), issueID, nil, &newDeadline); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update issue deadline",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to extend deadline")

		return
	}

	// Replace scheduler events with updated times.
	if err := s.store.DeleteEventsForIssue(r.Context(), issueID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to delete old scheduler events",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
	}

	extSettings, sErr := s.store.GetSettings(r.Context())
	if sErr != nil {
		s.logger.WarnContext(r.Context(), "Failed to load settings for extend — using UTC",
			slog.String("error", sErr.Error()))
	}

	evLoc := s.settingsLocation(r.Context(), extSettings)
	windowDuration := newDeadline.Sub(issue.OpensAt)
	reminder1At := atLocalHour(issue.OpensAt.Add(windowDuration/2), reminderLocalHour, evLoc)
	reminder2At := atLocalHour(newDeadline.AddDate(0, 0, -2), reminderLocalHour, evLoc)

	for _, ev := range []struct {
		eventType   string
		scheduledAt time.Time
	}{
		{"reminder_1", reminder1At},
		{"reminder_2", reminder2At},
		{"auto_close", newDeadline},
	} {
		if evErr := s.store.CreateSchedulerEvent(
			r.Context(), &issueID, ev.eventType, ev.scheduledAt,
		); evErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to create scheduler event after extend",
				slog.String("event_type", ev.eventType),
				slog.String("error", evErr.Error()))
		}
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

	if _, err := s.store.GetQuestionByID(r.Context(), questionID); err != nil {
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

	if err := s.store.UpdateQuestion(r.Context(), questionID, req.Text); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to update question",
			slog.Int64("question_id", questionID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update question")

		return
	}

	question, err := s.store.GetQuestionByID(r.Context(), questionID)
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

	if _, err := s.store.GetQuestionByID(r.Context(), questionID); err != nil {
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
		r.Context(), category, used, pg.Page, pg.PerPage,
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

	questionID, err := s.store.CreateBankQuestion(r.Context(), req.Text, req.Category)
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

	if err := s.store.UpdateBankQuestion(r.Context(), questionID, req.Text, req.Category); err != nil {
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

	if err := s.store.DeleteBankQuestion(r.Context(), questionID); err != nil {
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
	users []store.User,
	settings *store.Settings,
) {
	if settings == nil {
		s.logger.ErrorContext(ctx, "Cannot send publish notifications: settings unavailable",
			slog.Int64("issue_id", issue.ID))
		return
	}

	monthStr := formatDate(issue.OpensAt)
	subject := fmt.Sprintf("%s — %s is published!", settings.LoopName, monthStr)

	recipients := make([]email.BatchRecipient, 0, len(users))

	for i := range users {
		u := &users[i]

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

		recipients = append(recipients, email.BatchRecipient{
			UserID:    u.ID,
			Email:     u.Email,
			Subject:   subject,
			HTMLBody:  s.renderPublishEmail(settings.LoopName, u.Name, monthStr),
			IssueID:   &issue.ID,
			EmailType: "published",
		})
	}

	s.emailer.SendBatch(ctx, recipients, func(batchCtx context.Context, rec email.BatchRecipient, sendErr error) {
		if sendErr != nil {
			errStr := sendErr.Error()
			_, _ = s.store.LogEmail(batchCtx, &rec.UserID, rec.IssueID, rec.EmailType, "failed", &errStr)
			s.logger.ErrorContext(batchCtx, "Failed to send publish notification",
				slog.Int64("user_id", rec.UserID),
				slog.String("error", errStr))

			return
		}

		_, _ = s.store.LogEmail(batchCtx, &rec.UserID, rec.IssueID, rec.EmailType, "sent", nil)
	})
}

// insertDefaultQuestions adds every enabled default question to an issue
// with source "default", starting at sortOrder, and returns how many were
// added. Failures are logged per question — a missing default prompt must
// not block a round from opening.
func (s *Server) insertDefaultQuestions(
	ctx context.Context, issueID int64, sortOrder int,
) int {
	defaults, err := s.store.ListEnabledDefaultQuestions(ctx)
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
func (s *Server) renderPublishEmail(loopName, recipientName, month string) string {
	return s.renderEmail("published.html", map[string]any{
		"LoopName":      loopName,
		"RecipientName": recipientName,
		"Month":         month,
		"CTA": map[string]any{
			"URL":   s.config.BaseURL + "/issues",
			"Label": "Read the Issue",
		},
	})
}
