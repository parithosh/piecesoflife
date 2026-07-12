package server

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/store"
)

// emailLogPageSize is how many recent email log entries the settings page
// shows in its #email-log section.
const emailLogPageSize = 50

// AdminDashboardData is the template data for the admin dashboard page.
type AdminDashboardData struct {
	PageData
	CurrentIssue *store.Issue
	Progress     *store.SubmissionProgress
	PastIssues   []store.Issue
	RecentEmails []store.EmailLog
	// NextIssueOpens is the pre-formatted local time the next round is
	// scheduled to open (from the pending create_next_issue event). Empty
	// when nothing is queued or an issue is already active.
	NextIssueOpens string
	// Questions are the current issue's prompts, editable in place from the
	// dashboard. QuestionAnswers maps question ID → how many members already
	// have a response to it, powering the "already answered" warning.
	Questions       []store.Question
	QuestionAnswers map[int64]int
	// RambleCounts maps member ID → journal days written in the current
	// diary window ("since the last issue") — a pulse on whether the Ramble
	// feature is being used. Counts only; content stays private.
	RambleCounts map[int64]int
}

// AdminMembersData is the template data for the member management page.
type AdminMembersData struct {
	PageData
	Members []store.GroupMember
}

// AdminSettingsData is the template data for the settings page.
type AdminSettingsData struct {
	PageData
	// Email delivery facts (from env config, read-only on this page) so the
	// admin can see what's configured and fire a test send against it.
	EmailProvider string
	EmailFrom     string
	// DefaultQuestions are the prompts stitched into every new issue, with
	// their global enabled switches.
	DefaultQuestions []store.DefaultQuestion
	// EmailLogs is the newest slice of the Loop's outgoing-email history —
	// the #email-log section the dashboard's "View log" link points at.
	// EmailLogTotal is the all-time count, shown when it exceeds the slice.
	EmailLogs     []store.EmailLog
	EmailLogTotal int
}

// AdminQuestionsData is the template data for the question bank management page.
type AdminQuestionsData struct {
	PageData
	Questions []store.QuestionBank
	Total     int
}

// AdminMemberSubmissionData is the template data for the per-member
// submission view: an admin's preview of one user's response to the
// current issue, including drafts.
type AdminMemberSubmissionData struct {
	PageData
	Member    store.User
	Issue     *store.Issue
	Questions []store.Question
	// Answers maps question ID → the user's response and its blocks for
	// that question. Missing entries mean the member hasn't started that
	// question; entries with IsDraft=true are unsubmitted work-in-progress.
	Answers map[int64]MemberAnswer
}

// MemberAnswer pairs a single response with its content blocks for the
// member-submission template.
type MemberAnswer struct {
	Response store.Response
	Blocks   []store.ResponseBlock
}

// handleAdminDashboard renders the admin dashboard.
// GET /admin
func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := currentGroupID(ctx)

	settings, ok := s.loadSettingsOr500(w, r)
	if !ok {
		return
	}

	currentIssue, err := s.store.GetActiveIssue(ctx, groupID)
	if err != nil {
		// No active issue is not a fatal error.
		s.logger.InfoContext(ctx, "No active issue found",
			slog.String("error", err.Error()))
	}

	var progress *store.SubmissionProgress
	var questions []store.Question
	var questionAnswers map[int64]int
	rambleCounts := make(map[int64]int, 8)

	if currentIssue != nil {
		progress, err = s.store.GetSubmissionProgress(ctx, currentIssue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to get submission progress",
				slog.Int64("issue_id", currentIssue.ID),
				slog.String("error", err.Error()))
		}

		// A per-member pulse on the Ramble feature: how many journal days
		// fall in this round's diary window. Counts only — never content.
		if progress != nil {
			fromDay, throughDay := s.diaryWindow(ctx, groupID)

			for _, m := range progress.Members {
				n, cErr := s.store.CountRambleDaysBetween(
					ctx, m.User.ID, fromDay, throughDay)
				if cErr != nil {
					s.logger.WarnContext(ctx, "Failed to count member rambles",
						slog.Int64("user_id", m.User.ID),
						slog.String("error", cErr.Error()))

					continue
				}

				rambleCounts[m.User.ID] = n
			}
		}

		questions, err = s.store.ListQuestionsByIssue(ctx, currentIssue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to list questions for dashboard",
				slog.Int64("issue_id", currentIssue.ID),
				slog.String("error", err.Error()))
			questions = nil
		}

		questionAnswers, err = s.store.CountResponsesByQuestion(ctx, currentIssue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to count responses by question",
				slog.Int64("issue_id", currentIssue.ID),
				slog.String("error", err.Error()))
			questionAnswers = map[int64]int{}
		}
	}

	publishedStatus := "published"
	pastIssues, err := s.store.ListIssues(ctx, groupID, &publishedStatus)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list past issues",
			slog.String("error", err.Error()))
		pastIssues = make([]store.Issue, 0)
	}

	recentEmails, _, err := s.store.ListEmailLogs(ctx, groupID, 1, 10)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list recent emails",
			slog.String("error", err.Error()))
		recentEmails = make([]store.EmailLog, 0)
	}

	var nextIssueOpens string
	if currentIssue == nil {
		nextIssueOpens = s.nextIssueOpensLabel(ctx, settings)
	}

	data := AdminDashboardData{
		PageData:        s.newPageData(r),
		CurrentIssue:    currentIssue,
		Progress:        progress,
		PastIssues:      pastIssues,
		RecentEmails:    recentEmails,
		NextIssueOpens:  nextIssueOpens,
		Questions:       questions,
		QuestionAnswers: questionAnswers,
		RambleCounts:    rambleCounts,
	}

	s.renderPage(w, "dashboard.html", data)
}

// handleAdminMembers renders the member management page.
// GET /admin/members
func (s *Server) handleAdminMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	members, err := s.store.ListGroupMembers(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list members",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	data := AdminMembersData{
		PageData: s.newPageData(r),
		Members:  members,
	}

	s.renderPage(w, "members.html", data)
}

// handleAdminMemberSubmission renders one member's response to the active
// issue, including drafts. Lets the admin preview work-in-progress that
// isn't visible from the public issue page yet.
// GET /admin/members/{userId}/submission
func (s *Server) handleAdminMemberSubmission(
	w http.ResponseWriter, r *http.Request,
) {
	ctx := r.Context()
	groupID := currentGroupID(ctx)

	memberID, err := strconv.ParseInt(r.PathValue("userId"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	member, err := s.store.GetUserByID(ctx, memberID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// The viewed member must belong to this Loop.
	if _, err := s.store.GetMembership(ctx, groupID, memberID); err != nil {
		http.NotFound(w, r)
		return
	}

	// "Current issue" = the active draft/collecting issue. Published issues
	// are already viewable on the regular issue page, so we don't redirect
	// or fall back to them here — the admin can use the archive for those.
	issue, err := s.store.GetActiveIssue(ctx, groupID)
	if err != nil {
		s.logger.InfoContext(ctx, "No active issue for member submission view",
			slog.Int64("member_id", memberID),
			slog.String("error", err.Error()))
	}

	data := AdminMemberSubmissionData{
		PageData: s.newPageData(r),
		Member:   *member,
		Issue:    issue,
		Answers:  make(map[int64]MemberAnswer, 0),
	}

	if issue != nil {
		questions, err := s.store.ListQuestionsByIssue(ctx, issue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to load questions for member submission",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)

			return
		}

		responses, err := s.store.ListUserResponsesForIssue(ctx, memberID, issue.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to load member responses",
				slog.Int64("member_id", memberID),
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)

			return
		}

		answers := make(map[int64]MemberAnswer, len(responses))

		for i := range responses {
			resp := responses[i]

			blocks, err := s.store.ListBlocksByResponse(ctx, resp.ID)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to load response blocks",
					slog.Int64("response_id", resp.ID),
					slog.String("error", err.Error()))
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)

				return
			}

			answers[resp.QuestionID] = MemberAnswer{Response: resp, Blocks: blocks}
		}

		data.Questions = questions
		data.Answers = answers
	}

	s.renderPage(w, "member_submission.html", data)
}

// handleAdminQuestions renders the question bank management page.
// GET /admin/questions
func (s *Server) handleAdminQuestions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pg := parsePagination(r)

	questions, total, err := s.store.ListQuestionBank(ctx, currentGroupID(ctx),
		nil, nil, pg.Page, pg.PerPage)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list question bank",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	data := AdminQuestionsData{
		PageData:  s.newPageData(r),
		Questions: questions,
		Total:     total,
	}

	s.renderPage(w, "questions.html", data)
}

// handleAdminSettings renders the settings page.
// GET /admin/settings
func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	groupID := currentGroupID(ctx)

	defaultQuestions, err := s.store.ListDefaultQuestions(ctx, groupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list default questions",
			slog.String("error", err.Error()))
		defaultQuestions = make([]store.DefaultQuestion, 0)
	}

	emailLogs, emailLogTotal, err := s.store.ListEmailLogs(ctx, groupID, 1, emailLogPageSize)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list email logs",
			slog.String("error", err.Error()))
		emailLogs = make([]store.EmailLog, 0)
	}

	data := AdminSettingsData{
		PageData:         s.newPageData(r),
		EmailProvider:    s.config.EmailProvider,
		EmailFrom:        s.config.FromEmail,
		DefaultQuestions: defaultQuestions,
		EmailLogs:        emailLogs,
		EmailLogTotal:    emailLogTotal,
	}

	s.renderPage(w, "settings.html", data)
}

// handleSendTestEmail sends a short test email to the requesting admin so the
// configured provider (SMTP or JMAP) can be verified end to end without
// waiting for a real reminder or publish to go out.
// POST /api/admin/email/test
func (s *Server) handleSendTestEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := UserFromContext(ctx)

	loopName := "PiecesOfLife"
	if gc := GroupFromContext(ctx); gc != nil && gc.Settings.LoopName != "" {
		loopName = gc.Settings.LoopName
	}

	subject := fmt.Sprintf("Test email from %s", loopName)
	body := fmt.Sprintf(
		`<p>This is a test email from <b>%s</b>.</p>
<p>Delivery provider: <b>%s</b> · From: <b>%s</b></p>
<p>If you're reading this, outgoing email is configured correctly.</p>`,
		html.EscapeString(loopName),
		html.EscapeString(s.config.EmailProvider),
		html.EscapeString(s.config.FromEmail),
	)

	if err := s.emailer.Send(ctx, user.Email, subject, body); err != nil {
		s.logger.ErrorContext(ctx, "Test email failed",
			slog.String("provider", s.config.EmailProvider),
			slog.String("to", user.Email),
			slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "email_failed", err.Error())

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"provider": s.config.EmailProvider,
		"to":       user.Email,
	})
}

// handleInviteUser invites a new member by email.
// POST /api/users/invite
func (s *Server) handleInviteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		Email string `json:"email"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeValidationError(w, map[string]string{"email": "Valid email is required"})
		return
	}

	gc := GroupFromContext(ctx)
	settings := gc.Settings
	groupID := gc.Group.ID
	inviter := UserFromContext(ctx)

	existingUser, err := s.store.GetUserByEmail(ctx, req.Email)
	if err == nil {
		// Known account: joining this Loop is just a membership. An active
		// existing membership is a conflict; a deactivated one (or none) is
		// (re)created. A globally deactivated account is restored — the
		// invite is explicit intent to bring the person back.
		if membership, mErr := s.store.GetMembership(ctx, groupID, existingUser.ID); mErr == nil &&
			membership.IsActive && existingUser.IsActive {
			writeError(w, http.StatusConflict, "already_member", "User is already an active member")
			return
		}

		if !existingUser.IsActive {
			if err := s.store.ReactivateUser(ctx, existingUser.ID); err != nil {
				s.logger.ErrorContext(ctx, "Failed to reactivate user",
					slog.String("email", req.Email),
					slog.String("error", err.Error()))
				writeError(w, http.StatusInternalServerError, "server_error", "Failed to reactivate user")
				return
			}
		}

		if err := s.store.CreateMembership(ctx, groupID, existingUser.ID, "member"); err != nil {
			s.logger.ErrorContext(ctx, "Failed to add member",
				slog.String("email", req.Email),
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to add member")
			return
		}

		s.sendInviteEmail(ctx, groupID, existingUser.ID, req.Email,
			settings.LoopName, inviter.Name, settings.InviteNote, nil)

		writeJSON(w, http.StatusOK, map[string]string{"message": "Invite sent"})
		return
	}

	// Create new user.
	name := strings.Split(req.Email, "@")[0]

	newUserID, err := s.store.CreateUser(ctx, name, req.Email)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to create invited user",
			slog.String("email", req.Email),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create user")
		return
	}

	if err := s.store.CreateMembership(ctx, groupID, newUserID, "member"); err != nil {
		s.logger.ErrorContext(ctx, "Failed to create membership",
			slog.String("email", req.Email),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to add member")
		return
	}

	if err := s.store.EnsureNotificationPreferences(ctx, newUserID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to create notification preferences",
			slog.Int64("user_id", newUserID),
			slog.String("error", err.Error()))
	}

	s.sendInviteEmail(ctx, groupID, newUserID, req.Email,
		settings.LoopName, inviter.Name, settings.InviteNote, nil)

	s.logger.InfoContext(ctx, "User invited",
		slog.String("email", req.Email),
		slog.Int64("user_id", newUserID))

	writeJSON(w, http.StatusCreated, map[string]string{"message": "Invite sent"})
}

// handleGetSettings returns the current settings as JSON.
// GET /api/admin/settings
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	settings, err := s.store.GetSettings(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to get settings")
		return
	}

	writeJSON(w, http.StatusOK, settings)
}

// handleUpdateSettings applies a partial settings update.
// PATCH /api/admin/settings
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req struct {
		LoopName             *string `json:"loop_name"`
		Tagline              *string `json:"tagline"`
		Frequency            *string `json:"frequency"`
		SubmissionWindowDays *int    `json:"submission_window_days"`
		Timezone             *string `json:"timezone"`
		InviteNote           *string `json:"invite_note"`
		AccentColor          *string `json:"accent_color"`
		AutoCreateEnabled    *bool   `json:"auto_create_enabled"`
		AllowPublicMementos  *bool   `json:"allow_public_mementos"`
		QuestionsPerIssue    *int    `json:"questions_per_issue"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	current, err := s.store.GetSettings(ctx, currentGroupID(ctx))
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")
		return
	}

	// Apply partial updates.
	if req.LoopName != nil {
		trimmed := strings.TrimSpace(*req.LoopName)
		if trimmed == "" {
			writeValidationError(w, map[string]string{"loop_name": "Newsletter name cannot be empty"})
			return
		}
		current.LoopName = trimmed
	}

	if req.Tagline != nil {
		current.Tagline = req.Tagline
	}

	if req.Frequency != nil {
		validFreq := map[string]bool{"biweekly": true, "monthly": true, "quarterly": true}
		if !validFreq[*req.Frequency] {
			writeValidationError(w, map[string]string{
				"frequency": "Must be biweekly, monthly, or quarterly",
			})
			return
		}
		current.Frequency = *req.Frequency
	}

	if req.SubmissionWindowDays != nil {
		if *req.SubmissionWindowDays < 3 || *req.SubmissionWindowDays > 21 {
			writeValidationError(w, map[string]string{
				"submission_window_days": "Must be between 3 and 21 days",
			})
			return
		}
		current.SubmissionWindowDays = *req.SubmissionWindowDays
	}

	if req.Timezone != nil {
		if _, tzErr := time.LoadLocation(*req.Timezone); tzErr != nil {
			writeValidationError(w, map[string]string{
				"timezone": "Unknown timezone — use an IANA name like Asia/Kolkata or Europe/Berlin",
			})

			return
		}
		current.Timezone = *req.Timezone
	}

	if req.InviteNote != nil {
		current.InviteNote = req.InviteNote
	}

	if req.AccentColor != nil {
		color := strings.TrimSpace(*req.AccentColor)
		if !isValidHexColor(color) {
			writeValidationError(w, map[string]string{
				"accent_color": "Must be a 3 or 6 digit hex color, e.g. #2d5016",
			})
			return
		}
		current.AccentColor = color
	}

	if req.AutoCreateEnabled != nil {
		current.AutoCreateEnabled = *req.AutoCreateEnabled
	}

	if req.AllowPublicMementos != nil {
		current.AllowPublicMementos = *req.AllowPublicMementos
	}

	if req.QuestionsPerIssue != nil {
		if *req.QuestionsPerIssue < 1 || *req.QuestionsPerIssue > 20 {
			writeValidationError(w, map[string]string{
				"questions_per_issue": "Must be between 1 and 20 questions",
			})
			return
		}
		current.QuestionsPerIssue = *req.QuestionsPerIssue
	}

	if err := s.store.UpdateSettings(ctx, current); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update settings")
		return
	}

	s.logger.InfoContext(ctx, "Settings updated")

	writeJSON(w, http.StatusOK, current)
}

// handleEmailLog returns a paginated list of email log entries.
// GET /api/admin/email-log
func (s *Server) handleEmailLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pg := parsePagination(r)

	logs, total, err := s.store.ListEmailLogs(ctx, currentGroupID(ctx), pg.Page, pg.PerPage)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to list email logs",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to list email logs")
		return
	}

	writeJSON(w, http.StatusOK, ListResponse[store.EmailLog]{
		Items:   logs,
		Total:   total,
		Page:    pg.Page,
		PerPage: pg.PerPage,
	})
}

// handleResendEmail resends a failed email from the log.
// POST /api/admin/resend/{logId}
func (s *Server) handleResendEmail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	logIDStr := r.PathValue("logId")

	logID, err := strconv.ParseInt(logIDStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "Invalid log ID")
		return
	}

	logEntry, err := s.store.GetEmailLogByID(ctx, logID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get email log entry",
			slog.Int64("log_id", logID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusNotFound, "not_found", "Email log entry not found")
		return
	}

	groupID := currentGroupID(ctx)
	if logEntry.GroupID == nil || *logEntry.GroupID != groupID {
		writeError(w, http.StatusNotFound, "not_found", "Email log entry not found")
		return
	}

	if logEntry.UserID == nil {
		writeError(w, http.StatusUnprocessableEntity, "no_recipient", "Log entry has no associated user")
		return
	}

	user, err := s.store.GetUserByID(ctx, *logEntry.UserID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get user for email resend",
			slog.Int64("user_id", *logEntry.UserID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to find recipient")
		return
	}

	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")
		return
	}

	// Generate a fresh auth token for the email CTA link.
	raw, err := s.mintEmailCTAToken(ctx, user.ID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to mint auth token",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create token")
		return
	}

	var subject, body string
	authURL := fmt.Sprintf("%s/?auth=%s&g=%d", s.config.BaseURL, raw, groupID)

	switch logEntry.Type {
	case "invite":
		inviter := UserFromContext(ctx)
		subject = fmt.Sprintf("You're invited to %s!", settings.LoopName)
		body = s.renderInviteEmail(settings.LoopName, inviter.Name, "", authURL)
	case "open":
		var questions []store.Question
		if logEntry.IssueID != nil {
			questions, err = s.store.ListQuestionsByIssue(ctx, *logEntry.IssueID)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to list questions for resend",
					slog.Int64("issue_id", *logEntry.IssueID),
					slog.String("error", err.Error()))
				questions = make([]store.Question, 0)
			}

			authURL = fmt.Sprintf("%s/issues/%d/respond?auth=%s",
				s.config.BaseURL, *logEntry.IssueID, raw)
		}

		var issue *store.Issue
		if logEntry.IssueID != nil {
			issue, err = s.store.GetIssueByID(ctx, *logEntry.IssueID)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to get issue for resend",
					slog.Int64("issue_id", *logEntry.IssueID),
					slog.String("error", err.Error()))
			}
		}

		monthStr := "this month"
		if issue != nil {
			monthStr = formatDate(issue.OpensAt)
		}

		questionTexts := make([]string, 0, len(questions))
		for _, q := range questions {
			questionTexts = append(questionTexts, q.Text)
		}

		subject = fmt.Sprintf("%s — %s is open!", settings.LoopName, monthStr)
		body = s.renderIssueOpenEmail(settings.LoopName, user.Name, monthStr, questionTexts, authURL)
	default:
		writeError(w, http.StatusUnprocessableEntity, "unsupported_type",
			fmt.Sprintf("Resend not supported for email type: %s", logEntry.Type))
		return
	}

	newLogID, _ := s.store.LogEmail(ctx, &groupID, &user.ID, logEntry.IssueID,
		logEntry.Type, "pending", nil)

	bgCtx := context.WithoutCancel(ctx)

	go func() {
		sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()

		sendErr := s.emailer.Send(sendCtx, user.Email, subject, body)
		if sendErr != nil {
			errStr := sendErr.Error()
			if newLogID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, newLogID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(sendCtx, "Failed to resend email",
				slog.Int64("original_log_id", logID),
				slog.String("email", user.Email),
				slog.String("error", errStr))
		} else {
			now := time.Now()
			if newLogID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, newLogID, "sent", &now, nil)
			}
		}
	}()

	s.logger.InfoContext(ctx, "Email resend queued",
		slog.Int64("original_log_id", logID),
		slog.String("type", logEntry.Type),
		slog.Int64("user_id", user.ID))

	writeJSON(w, http.StatusAccepted, map[string]string{"message": "Email resend queued"})
}

// handleSendReminder sends a reminder email to members who have not yet responded.
// POST /api/admin/send-reminder/{issueId}
//
// The actual email batch is delegated to SendReminderForIssue (also used by
// the scheduler). This handler just validates input, kicks off the batch in
// a detached goroutine, and returns 202 Accepted.
func (s *Server) handleSendReminder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	issueID, ok := s.parseIDParam(w, r, "issueId", "issue ID")
	if !ok {
		return
	}

	issue, ok := s.requireIssue(w, r, issueID)
	if !ok {
		return
	}

	if issue.Status != "collecting" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_status",
			"Reminders can only be sent for issues in collecting status")
		return
	}

	bgCtx := context.WithoutCancel(ctx)
	go func() {
		if err := s.SendReminderForIssue(bgCtx, issueID, false, nil); err != nil {
			s.logger.ErrorContext(bgCtx, "Manual reminder batch failed",
				slog.Int64("issue_id", issueID),
				slog.String("error", err.Error()))
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"message": "Reminder emails queued",
	})
}

// isValidHexColor reports whether s is a valid #RGB or #RRGGBB color.
func isValidHexColor(s string) bool {
	if len(s) != 4 && len(s) != 7 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
