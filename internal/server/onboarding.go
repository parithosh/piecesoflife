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

	"github.com/parithosh/piecesoflife/internal/store"
)

// SetupWizardData is the template data for the onboarding setup page.
type SetupWizardData struct {
	PageData
	SuggestedQuestions []store.QuestionBank
	// DefaultQuestions are the prompts stitched into every issue; the wizard
	// offers the same global on/off switches as the settings page.
	DefaultQuestions []store.DefaultQuestion
}

// handleAdminSetup renders the onboarding wizard page.
// GET /admin/setup
func (s *Server) handleAdminSetup(w http.ResponseWriter, r *http.Request) {
	setupComplete, err := s.store.IsSetupComplete(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check setup status",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if setupComplete {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	user := UserFromContext(r.Context())
	settings, _ := s.store.GetSettings(r.Context())

	defaultQuestions, err := s.store.ListDefaultQuestions(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to list default questions",
			slog.String("error", err.Error()))
		defaultQuestions = make([]store.DefaultQuestion, 0)
	}

	// The default questions already fill part of the target, so only suggest
	// enough bank picks to cover the rest of the first issue.
	suggestCount := questionTarget(settings, 0)
	for _, dq := range defaultQuestions {
		if dq.Enabled {
			suggestCount--
		}
	}

	if suggestCount < 1 {
		suggestCount = 1
	}

	questions, err := s.store.SelectRandomUnusedQuestions(r.Context(), suggestCount)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to select suggested questions",
			slog.String("error", err.Error()))
	}

	data := SetupWizardData{
		PageData: PageData{
			User:     user,
			Settings: settings,
		},
		SuggestedQuestions: questions,
		DefaultQuestions:   defaultQuestions,
	}

	s.renderPage(w, "setup.html", data)
}

// onboardingRequest is the expected JSON body for POST /api/onboarding/complete.
type onboardingRequest struct {
	AdminName            string                      `json:"admin_name"`
	LoopName             string                      `json:"loop_name"`
	Tagline              *string                     `json:"tagline"`
	Frequency            string                      `json:"frequency"`
	SubmissionWindowDays int                         `json:"submission_window_days"`
	StartDatetime        string                      `json:"start_datetime"`
	Timezone             string                      `json:"timezone"`
	QuestionsPerIssue    int                         `json:"questions_per_issue"`
	DefaultQuestions     []onboardingDefaultQuestion `json:"default_questions"`
	Questions            []onboardingQuestion        `json:"questions"`
	InviteEmails         []string                    `json:"invite_emails"`
	InviteNote           *string                     `json:"invite_note"`
}

type onboardingQuestion struct {
	Text     string  `json:"text"`
	Category *string `json:"category"`
	BankID   *int64  `json:"bank_id"`
	// MakeDefault also saves this pick as a global default question, so it
	// is stitched into every future issue rather than just the first one.
	MakeDefault bool `json:"make_default"`
}

// onboardingDefaultQuestion carries the wizard's global on/off choice for
// one default question.
type onboardingDefaultQuestion struct {
	ID      int64 `json:"id"`
	Enabled bool  `json:"enabled"`
}

// handleCompleteOnboarding processes the setup wizard submission.
// POST /api/onboarding/complete
func (s *Server) handleCompleteOnboarding(w http.ResponseWriter, r *http.Request) {
	// Idempotency check.
	setupComplete, err := s.store.IsSetupComplete(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")
		return
	}

	if setupComplete {
		writeJSON(w, http.StatusOK, map[string]string{"redirect": "/admin"})
		return
	}

	var req onboardingRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	// Validate required fields.
	errors := make(map[string]string, 4)
	if strings.TrimSpace(req.AdminName) == "" {
		errors["admin_name"] = "Name is required"
	}
	if strings.TrimSpace(req.LoopName) == "" {
		errors["loop_name"] = "Newsletter name is required"
	}
	if req.StartDatetime == "" {
		errors["start_datetime"] = "Start date is required"
	}

	validFreq := map[string]bool{"biweekly": true, "monthly": true, "quarterly": true}
	if !validFreq[req.Frequency] {
		errors["frequency"] = "Must be biweekly, monthly, or quarterly"
	}

	if req.SubmissionWindowDays < 3 || req.SubmissionWindowDays > 21 {
		errors["submission_window_days"] = "Must be between 3 and 21 days"
	}

	// Zero means "not provided" and keeps the seeded default.
	if req.QuestionsPerIssue < 0 || req.QuestionsPerIssue > 20 {
		errors["questions_per_issue"] = "Must be between 1 and 20 questions"
	}

	if len(req.Questions) == 0 {
		errors["questions"] = "At least one question is required"
	}

	if len(errors) > 0 {
		writeValidationError(w, errors)
		return
	}

	// Parse start datetime in the provided timezone. An unknown zone is a
	// validation error — silently falling back to UTC would skew every
	// scheduled reminder for the life of the loop.
	loc, err := time.LoadLocation(req.Timezone)
	if err != nil {
		writeValidationError(w, map[string]string{
			"timezone": "Unknown timezone — use an IANA name like Asia/Kolkata or Europe/Berlin",
		})

		return
	}

	localTime, err := time.ParseInLocation("2006-01-02T15:04:05", req.StartDatetime, loc)
	if err != nil {
		localTime, err = time.ParseInLocation("2006-01-02T15:04", req.StartDatetime, loc)
		if err != nil {
			writeValidationError(w, map[string]string{
				"start_datetime": "Invalid date format",
			})
			return
		}
	}

	utcStart := localTime.UTC()
	deadline := utcStart.Add(time.Duration(req.SubmissionWindowDays) * 24 * time.Hour)

	user := UserFromContext(r.Context())
	ctx := r.Context()
	month := int(utcStart.Month())
	year := utcStart.Year()

	// The sequence below is written to be *resumable*. If an earlier attempt
	// failed partway through, submitting the wizard again converges to the
	// same state instead of creating duplicates. Each step either:
	//   (a) is already idempotent at the SQL level (UPDATE / UPSERT), or
	//   (b) reuses existing rows when it finds them (issue lookup), or
	//   (c) deletes-and-recreates a self-contained slice of state
	//       (questions, scheduler events) that is safe to rewrite because
	//       no downstream rows can exist before setup_complete is set.
	// setup_complete is flipped last — until then, the wizard stays open.

	// Step 1: Update admin name. Idempotent (UPDATE).
	if err := s.store.SetUserName(ctx, user.ID, req.AdminName); err != nil {
		s.logger.ErrorContext(ctx, "Failed to set admin name",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update name")
		return
	}

	// Step 2: Update settings. Idempotent (single-row UPSERT-ish).
	currentSettings, _ := s.store.GetSettings(ctx)
	accentColor := "#2d5016"
	autoCreateEnabled := false
	allowPublicMementos := true
	questionsPerIssue := fallbackQuestionsPerIssue
	if currentSettings != nil {
		if currentSettings.AccentColor != "" {
			accentColor = currentSettings.AccentColor
		}
		autoCreateEnabled = currentSettings.AutoCreateEnabled
		allowPublicMementos = currentSettings.AllowPublicMementos
		if currentSettings.QuestionsPerIssue > 0 {
			questionsPerIssue = currentSettings.QuestionsPerIssue
		}
	}

	if req.QuestionsPerIssue > 0 {
		questionsPerIssue = req.QuestionsPerIssue
	}

	settings := &store.Settings{
		LoopName:             req.LoopName,
		Tagline:              req.Tagline,
		Frequency:            req.Frequency,
		SubmissionWindowDays: req.SubmissionWindowDays,
		StartDatetime:        &utcStart,
		Timezone:             req.Timezone,
		InviteNote:           req.InviteNote,
		AccentColor:          accentColor,
		AutoCreateEnabled:    autoCreateEnabled,
		AllowPublicMementos:  allowPublicMementos,
		QuestionsPerIssue:    questionsPerIssue,
	}

	if err := s.store.UpdateSettings(ctx, settings); err != nil {
		s.logger.ErrorContext(ctx, "Failed to update settings",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to update settings")
		return
	}

	// Step 2b: Apply the wizard's default-question switches so the first
	// issue (and every later one) honors them. Idempotent (UPDATE WHERE id=);
	// a bad ID is logged rather than blocking the launch.
	for _, dq := range req.DefaultQuestions {
		if err := s.store.SetDefaultQuestionEnabled(ctx, dq.ID, dq.Enabled); err != nil {
			s.logger.WarnContext(ctx, "Failed to apply default question switch",
				slog.Int64("default_question_id", dq.ID),
				slog.Bool("enabled", dq.Enabled),
				slog.String("error", err.Error()))
		}
	}

	// The wizard's list order is the admin's chosen order — persist it
	// globally so future issues stitch the defaults in the same sequence.
	// Best-effort: a stale list (e.g. a retry after a pick was promoted to
	// a default) must not block the launch.
	if len(req.DefaultQuestions) > 0 {
		ids := make([]int64, 0, len(req.DefaultQuestions))
		for _, dq := range req.DefaultQuestions {
			ids = append(ids, dq.ID)
		}

		if err := s.store.ReorderDefaultQuestions(ctx, ids); err != nil {
			s.logger.WarnContext(ctx, "Failed to apply default question order from wizard",
				slog.String("error", err.Error()))
		}
	}

	// Step 3: Mark chosen bank questions as used. Idempotent (UPDATE WHERE id=).
	for _, q := range req.Questions {
		if q.BankID != nil {
			if err := s.store.MarkBankQuestionUsed(ctx, *q.BankID); err != nil {
				s.logger.ErrorContext(ctx, "Failed to mark bank question used",
					slog.Int64("bank_id", *q.BankID),
					slog.String("error", err.Error()))
			}
		}
	}

	// Step 4: Get-or-create the setup issue. On retry, reuse the issue from
	// a previous partial attempt instead of creating a second one for the
	// same (month, year).
	issueID, err := s.getOrCreateSetupIssue(ctx, month, year, utcStart, deadline)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to get or create setup issue",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to create issue")
		return
	}

	// Step 5: Ensure status is "collecting". Idempotent.
	if err := s.store.SetIssueStatus(ctx, issueID, "collecting"); err != nil {
		s.logger.ErrorContext(ctx, "Failed to set issue status",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to set issue status")
		return
	}

	// Step 6: Replace questions for the issue. Delete any stale ones left
	// from a prior attempt, then insert the current set. Safe because
	// setup_complete has not been set yet, so no responses exist.
	if err := s.store.DeleteQuestionsByIssue(ctx, issueID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to clear existing questions",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to reset questions")
		return
	}

	// Enabled default questions lead the first round; the admin's picks follow.
	numDefaults := s.insertDefaultQuestions(ctx, issueID, 0)

	// A retried wizard may have already promoted picks to global defaults
	// (Step 6b) — insertDefaultQuestions just stitched those in, so skip
	// re-inserting them as picks.
	alreadyInserted := make(map[string]bool, numDefaults)
	if inserted, listErr := s.store.ListQuestionsByIssue(ctx, issueID); listErr == nil {
		for _, q := range inserted {
			alreadyInserted[strings.TrimSpace(q.Text)] = true
		}
	}

	sortOrder := numDefaults

	for _, q := range req.Questions {
		if alreadyInserted[strings.TrimSpace(q.Text)] {
			continue
		}

		source := "bank"
		if q.BankID == nil {
			source = "friend"
		}

		if _, err := s.store.CreateQuestion(
			ctx, issueID, q.Text, q.Category, source, nil, sortOrder,
		); err != nil {
			s.logger.ErrorContext(ctx, "Failed to create question",
				slog.String("text", q.Text),
				slog.String("error", err.Error()))
			writeError(w, http.StatusInternalServerError, "server_error", "Failed to create question")
			return
		}

		sortOrder++
	}

	// Step 6b: Promote flagged picks to global default questions so every
	// future issue carries them. Duplicate text means an existing default
	// (or a previous attempt) already covers it — not an error.
	for _, q := range req.Questions {
		if !q.MakeDefault {
			continue
		}

		text := strings.TrimSpace(q.Text)
		if text == "" {
			continue
		}

		if _, err := s.store.CreateDefaultQuestion(ctx, text); err != nil &&
			!strings.Contains(err.Error(), "UNIQUE") {
			s.logger.WarnContext(ctx, "Failed to save wizard pick as default question",
				slog.String("text", text),
				slog.String("error", err.Error()))
		}
	}

	// Step 7: Replace scheduler events. Delete any pending ones for this
	// issue and recreate — deadline/window may have changed between attempts,
	// and the UNIQUE(issue_id, event_type, scheduled_at) constraint would
	// otherwise make naive re-insertion fail.
	if err := s.store.DeleteEventsForIssue(ctx, issueID); err != nil {
		s.logger.ErrorContext(ctx, "Failed to clear scheduler events",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to reset scheduler")
		return
	}

	s.scheduleIssueEvents(ctx, settings, issueID, deadline)

	// Step 8: Invite friends. Each invite self-guards against existing
	// active users, so retries don't create duplicates at the user level.
	// The side effect — extra invite emails on retry — is preferred over
	// leaving new members uninvited.
	for _, email := range req.InviteEmails {
		email = strings.TrimSpace(strings.ToLower(email))
		if email == "" || !strings.Contains(email, "@") {
			continue
		}

		existingUser, err := s.store.GetUserByEmail(ctx, email)
		if err == nil {
			if existingUser.IsActive {
				// Active from a prior attempt — re-send the invite so the
				// friend still gets a working CTA link.
				s.sendInviteEmail(ctx, existingUser.ID, email,
					req.LoopName, req.AdminName, req.InviteNote, &issueID)
				continue
			}

			if err := s.store.ReactivateUser(ctx, existingUser.ID); err != nil {
				s.logger.ErrorContext(ctx, "Failed to reactivate user",
					slog.String("email", email),
					slog.String("error", err.Error()))
			}

			s.sendInviteEmail(ctx, existingUser.ID, email,
				req.LoopName, req.AdminName, req.InviteNote, &issueID)

			continue
		}

		// Create new user.
		name := strings.Split(email, "@")[0]

		newUserID, err := s.store.CreateUser(ctx, name, email, "member")
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to create invited user",
				slog.String("email", email),
				slog.String("error", err.Error()))
			continue
		}

		if err := s.store.EnsureNotificationPreferences(ctx, newUserID); err != nil {
			s.logger.ErrorContext(ctx, "Failed to create notification prefs",
				slog.Int64("user_id", newUserID),
				slog.String("error", err.Error()))
		}

		s.sendInviteEmail(ctx, newUserID, email,
			req.LoopName, req.AdminName, req.InviteNote, &issueID)
	}

	// Step 9: Mark setup complete. This is the commit point — until it
	// succeeds, the wizard remains open and resubmission re-converges.
	if err := s.store.CompleteSetup(ctx); err != nil {
		s.logger.ErrorContext(ctx, "Failed to complete setup",
			slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "server_error", "Failed to complete setup")
		return
	}

	s.logger.InfoContext(ctx, "Onboarding completed",
		slog.String("loop_name", req.LoopName),
		slog.Int64("issue_id", issueID),
		slog.Int("invite_count", len(req.InviteEmails)),
	)

	writeJSON(w, http.StatusOK, map[string]string{"redirect": "/admin"})
}

// getOrCreateSetupIssue returns the issue for (month, year) if one already
// exists from a prior onboarding attempt; otherwise it creates a fresh one.
// On reuse it also refreshes opens_at/deadline so wizard edits take effect.
func (s *Server) getOrCreateSetupIssue(
	ctx context.Context,
	month, year int,
	opensAt, deadline time.Time,
) (int64, error) {
	existing, err := s.store.GetIssueByMonthYear(ctx, month, year)
	if err == nil {
		// Refresh schedule in case the admin changed start/window on retry.
		// UpdateIssue uses COALESCE so passing non-nil deadline updates it;
		// opens_at is not exposed by UpdateIssue and is acceptable as-is
		// because the UI does not allow back-dating a started issue.
		if updErr := s.store.UpdateIssue(ctx, existing.ID, nil, &deadline); updErr != nil {
			return 0, fmt.Errorf("refreshing setup issue deadline: %w", updErr)
		}

		return existing.ID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("looking up existing setup issue: %w", err)
	}

	return s.store.CreateIssue(ctx, nil, month, year, opensAt, deadline)
}

// sendInviteEmail generates a CTA token and sends an invite email to a user.
func (s *Server) sendInviteEmail(
	ctx context.Context,
	userID int64, email, loopName, adminName string,
	inviteNote *string, issueID *int64,
) {
	raw, err := s.mintEmailCTAToken(ctx, userID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to mint invite token",
			slog.String("error", err.Error()))
		return
	}

	authURL := s.config.BaseURL + "/?auth=" + raw
	note := ""
	if inviteNote != nil {
		note = *inviteNote
	}

	subject := fmt.Sprintf("You're invited to %s!", loopName)
	body := s.renderInviteEmail(loopName, adminName, note, authURL)

	// Log the email attempt.
	logID, _ := s.store.LogEmail(ctx, &userID, issueID, "invite", "pending", nil)

	// Detach ctx so the SMTP dial + DB log update survive after this request.
	bgCtx := context.WithoutCancel(ctx)

	go func() {
		sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()

		sendErr := s.emailer.Send(sendCtx, email, subject, body)
		if sendErr != nil {
			errStr := sendErr.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(sendCtx, "Failed to send invite email",
				slog.String("email", email),
				slog.String("error", errStr))
		} else {
			now := time.Now()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, logID, "sent", &now, nil)
			}
		}
	}()
}

// renderInviteEmail builds the invite email using the invite.html template.
// html/template auto-escapes every data field — no manual escaping needed.
func (s *Server) renderInviteEmail(loopName, adminName, note, authURL string) string {
	return s.renderEmail("invite.html", map[string]any{
		"LoopName":  loopName,
		"AdminName": adminName,
		"Note":      note,
		"CTA": map[string]any{
			"URL":   authURL,
			"Label": "Join " + loopName,
		},
	})
}

// sendIssueOpenEmail sends the "new issue" notification to a user.
func (s *Server) sendIssueOpenEmail(
	ctx context.Context,
	user *store.User, issue *store.Issue,
	settings *store.Settings, questions []store.Question,
) {
	// Check notification preferences.
	prefs, err := s.store.GetNotificationPreferences(ctx, user.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}

	if prefs != nil && !prefs.IssueOpen {
		return
	}

	raw, err := s.mintEmailCTAToken(ctx, user.ID)
	if err != nil {
		return
	}

	authURL := fmt.Sprintf("%s/issues/%d/respond?auth=%s", s.config.BaseURL, issue.ID, raw)
	monthStr := formatDate(issue.OpensAt)

	questionTexts := make([]string, 0, len(questions))
	for _, q := range questions {
		questionTexts = append(questionTexts, q.Text)
	}

	subject := fmt.Sprintf("%s — %s is open!", settings.LoopName, monthStr)
	body := s.renderIssueOpenEmail(settings.LoopName, user.Name, monthStr, questionTexts, authURL)

	logID, _ := s.store.LogEmail(ctx, &user.ID, &issue.ID, "open", "pending", nil)

	bgCtx := context.WithoutCancel(ctx)

	go func() {
		sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()

		sendErr := s.emailer.Send(sendCtx, user.Email, subject, body)
		if sendErr != nil {
			errStr := sendErr.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, logID, "failed", nil, &errStr)
			}
		} else {
			now := time.Now()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(sendCtx, logID, "sent", &now, nil)
			}
		}
	}()
}

// renderIssueOpenEmail builds the "issue is open" email using the
// issue_open.html template.
func (s *Server) renderIssueOpenEmail(
	loopName, recipientName, month string,
	questions []string, authURL string,
) string {
	return s.renderEmail("issue_open.html", map[string]any{
		"LoopName":      loopName,
		"RecipientName": recipientName,
		"Month":         month,
		"Questions":     questions,
		"CTA": map[string]any{
			"URL":   authURL,
			"Label": "Share Your Answers",
		},
	})
}
