package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

// reminderLocalHour and deadlineLocalHour pin scheduled emails and default
// deadlines to humane wall-clock times in the loop's configured timezone:
// reminders arrive mid-morning, issues close in the evening.
const (
	reminderLocalHour = 10
	deadlineLocalHour = 21
	nextOpenLocalHour = 9
)

// settingsLocation resolves the loop's configured timezone. Empty or invalid
// values fall back to UTC with a warning so scheduling never fails outright.
func (s *Server) settingsLocation(
	ctx context.Context, settings *store.Settings,
) *time.Location {
	if settings == nil || settings.Timezone == "" {
		return time.UTC
	}

	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		s.logger.WarnContext(ctx, "Invalid settings timezone — falling back to UTC",
			slog.String("timezone", settings.Timezone),
			slog.String("error", err.Error()))
		return time.UTC
	}

	return loc
}

// atLocalHour pins an instant to a specific wall-clock hour of the same
// calendar day in loc, so scheduled events land at a predictable local time.
func atLocalHour(t time.Time, hour int, loc *time.Location) time.Time {
	lt := t.In(loc)
	return time.Date(lt.Year(), lt.Month(), lt.Day(), hour, 0, 0, 0, loc)
}

// SendReminderForIssue sends a reminder email to every active member who has
// not yet submitted a response. If isFinal is true the subject uses a
// "last chance" phrasing. Each send is bounded by a 30s per-email timeout.
//
// This method blocks until all sends have been attempted. Callers that need
// non-blocking behaviour (the admin HTTP handler) should wrap it in a
// goroutine with a detached context. The scheduler already runs in its own
// goroutine so it calls this directly.
func (s *Server) SendReminderForIssue(
	ctx context.Context, issueID int64, isFinal bool, schedulerEventID *int64,
) error {
	issue, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading issue %d: %w", issueID, err)
	}

	if issue.Status != "collecting" {
		return fmt.Errorf("issue %d is not collecting", issueID)
	}

	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	questions, err := s.store.ListQuestionsByIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading questions for issue %d: %w", issueID, err)
	}

	progress, err := s.store.GetSubmissionProgress(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading progress for issue %d: %w", issueID, err)
	}

	nonResponders := make([]store.User, 0, len(progress.Members))
	for _, mp := range progress.Members {
		if !mp.Responded {
			nonResponders = append(nonResponders, mp.User)
		}
	}

	if len(nonResponders) == 0 {
		s.logger.InfoContext(ctx, "Reminder skipped — everyone has responded",
			slog.Int64("issue_id", issueID))
		return nil
	}

	questionTexts := make([]string, 0, len(questions))
	for _, q := range questions {
		questionTexts = append(questionTexts, q.Text)
	}

	monthStr := issue.OpensAt.Format("January 2006")
	daysLeft := int(time.Until(issue.Deadline).Hours() / 24)
	if daysLeft < 0 {
		daysLeft = 0
	}

	subjectPrefix := "Reminder"
	if isFinal {
		subjectPrefix = "Last chance"
	}
	subject := fmt.Sprintf("%s: %s — %s", subjectPrefix, settings.LoopName, monthStr)

	for _, user := range nonResponders {
		if err := ctx.Err(); err != nil {
			return err
		}

		prefs, prefErr := s.store.GetNotificationPreferences(ctx, user.ID)
		if prefErr == nil && prefs != nil && !prefs.Reminders {
			continue
		}

		var logID int64
		if schedulerEventID != nil {
			var shouldSend bool
			logID, shouldSend, err = s.store.BeginSchedulerEmailAttempt(
				ctx, *schedulerEventID, user.ID, &issueID, "reminder",
			)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to reserve reminder email log",
					slog.Int64("user_id", user.ID),
					slog.Int64("scheduler_event_id", *schedulerEventID),
					slog.String("error", err.Error()))
				continue
			}
			if !shouldSend {
				s.logger.InfoContext(ctx, "Reminder already sent for scheduler event",
					slog.Int64("user_id", user.ID),
					slog.Int64("scheduler_event_id", *schedulerEventID))
				continue
			}
		} else {
			logID, _ = s.store.LogEmail(ctx, &user.ID, &issueID, "reminder", "pending", nil)
		}

		raw, hash, err := auth.GenerateRandomToken(32)
		if err != nil {
			errStr := err.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to generate reminder token",
				slog.Int64("user_id", user.ID),
				slog.String("error", err.Error()))
			continue
		}

		expiresAt := time.Now().Add(30 * 24 * time.Hour)
		if err := s.store.CreateAuthToken(ctx, user.ID, hash, "email_cta", expiresAt); err != nil {
			errStr := err.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to create reminder auth token",
				slog.Int64("user_id", user.ID),
				slog.String("error", err.Error()))
			continue
		}

		authURL := fmt.Sprintf("%s/issues/%d/respond?auth=%s",
			s.config.BaseURL, issueID, raw)

		body := s.renderReminderEmail(
			settings.LoopName, user.Name, monthStr,
			questionTexts, authURL, isFinal, daysLeft,
			progress.Responded, progress.TotalMembers,
		)

		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sendErr := s.emailer.Send(sendCtx, user.Email, subject, body)
		cancel()

		if sendErr != nil {
			errStr := sendErr.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to send reminder email",
				slog.Int64("user_id", user.ID),
				slog.String("error", errStr))
			continue
		}

		now := time.Now()
		if logID > 0 {
			_ = s.store.UpdateEmailLog(ctx, logID, "sent", &now, nil)
		}
	}

	s.logger.InfoContext(ctx, "Reminder batch completed",
		slog.Int64("issue_id", issueID),
		slog.Int("recipient_count", len(nonResponders)),
		slog.Bool("is_final", isFinal),
	)

	return nil
}

// AutoPublishIssue publishes an issue and sends the "published" notification
// to all members who have the published preference enabled. Used by the
// scheduler when an issue's auto_close event fires.
//
// The method is idempotent: if the issue is already published, it returns
// nil without re-sending notifications.
func (s *Server) AutoPublishIssue(ctx context.Context, issueID int64) error {
	issue, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading issue %d: %w", issueID, err)
	}

	if issue.Status == "published" {
		return nil
	}

	if err := s.store.PublishIssue(ctx, issueID); err != nil {
		return fmt.Errorf("publishing issue %d: %w", issueID, err)
	}

	users, err := s.store.ListActiveUsers(ctx)
	if err != nil {
		// Publish succeeded; notification failure shouldn't undo it.
		s.logger.ErrorContext(ctx, "Failed to list users for auto-publish notifications",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		return nil
	}

	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings for auto-publish",
			slog.String("error", err.Error()))
		return nil
	}

	updated, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		updated = issue
	}

	s.sendPublishNotifications(ctx, updated, users, settings)

	// If auto-create is on, queue the next-issue event at a cadence derived
	// from the loop's frequency. The scheduler will fire it and CreateNextIssue
	// takes care of the rest. Failing to queue is logged but doesn't fail
	// the publish.
	if settings.AutoCreateEnabled {
		s.queueNextIssueEvent(ctx, settings, updated.OpensAt)
	}

	s.logger.InfoContext(ctx, "Issue auto-published",
		slog.Int64("issue_id", issueID),
	)

	return nil
}

// queueNextIssueEvent schedules the create_next_issue event that opens the
// following round, at a cadence derived from the loop's frequency and pinned
// to a friendly local morning hour — and pre-creates that round as a draft
// so members can suggest questions to it while reading the issue that just
// went out. The draft accepts no answers until the event opens it. Callers
// guard on settings.AutoCreateEnabled. Failures are logged, never returned —
// a publish must not fail because scheduling the next round hiccupped.
func (s *Server) queueNextIssueEvent(
	ctx context.Context, settings *store.Settings, lastOpensAt time.Time,
) {
	loc := s.settingsLocation(ctx, settings)
	nextOpen := nextIssueOpenTime(lastOpensAt, settings.Frequency, time.Now().UTC())

	// Open at a friendly local morning hour rather than whatever UTC
	// instant the cadence math produced.
	nextOpen = atLocalHour(nextOpen, nextOpenLocalHour, loc)
	if nextOpen.Before(time.Now().Add(12 * time.Hour)) {
		nextOpen = atLocalHour(nextOpen.AddDate(0, 0, 1), nextOpenLocalHour, loc)
	}

	if err := s.store.CreateSchedulerEvent(ctx, nil, "create_next_issue", nextOpen); err != nil {
		s.logger.WarnContext(ctx, "Failed to queue create_next_issue event",
			slog.Time("scheduled_at", nextOpen),
			slog.String("error", err.Error()))
	} else {
		s.logger.InfoContext(ctx, "Queued create_next_issue event",
			slog.Time("scheduled_at", nextOpen),
		)
	}

	existing, err := s.store.GetNextDraftIssue(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to check for existing draft issue",
			slog.String("error", err.Error()))
		return
	}
	if existing != nil {
		return
	}

	windowDays := settings.SubmissionWindowDays
	if windowDays < 3 {
		windowDays = 7
	}

	openLocal := nextOpen.In(loc)
	deadline := atLocalHour(openLocal.AddDate(0, 0, windowDays), deadlineLocalHour, loc)

	issueID, err := s.store.CreateIssue(ctx, nil,
		int(openLocal.Month()), openLocal.Year(), nextOpen, deadline)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to pre-create next draft issue",
			slog.String("error", err.Error()))
		return
	}

	s.logger.InfoContext(ctx, "Pre-created next issue as draft",
		slog.Int64("issue_id", issueID),
		slog.Time("opens_at", nextOpen))
}

// openIssueForCollecting flips a draft round to collecting: stitches in the
// enabled default questions, then pads the list up to the total question
// target with bank questions. Friend suggestions gathered while the round
// was upcoming stay first, and — like the defaults — count toward the
// target, so the bank only fills the shortfall; suggestions beyond the
// target simply mean no padding. Schedules the round's reminder and
// auto-close events, and emails every active member that it is open.
// questionCount overrides the settings target when > 0.
func (s *Server) openIssueForCollecting(
	ctx context.Context, settings *store.Settings, issue *store.Issue,
	questionCount int,
) error {
	loc := s.settingsLocation(ctx, settings)
	now := time.Now().UTC()

	existing, err := s.store.ListQuestionsByIssue(ctx, issue.ID)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to list questions before opening issue",
			slog.Int64("issue_id", issue.ID),
			slog.String("error", err.Error()))
		existing = nil
	}

	hasDefaults := false

	for _, q := range existing {
		if q.Source == "default" {
			hasDefaults = true
			break
		}
	}

	nextOrder := len(existing)
	if !hasDefaults {
		nextOrder += s.insertDefaultQuestions(ctx, issue.ID, nextOrder)
	}

	if need := questionTarget(settings, questionCount) - nextOrder; need > 0 {
		bankQuestions, err := s.store.SelectRandomUnusedQuestions(ctx, need)
		if err != nil {
			s.logger.WarnContext(ctx, "Failed to select bank questions for issue open",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", err.Error()))
		}

		for i, bq := range bankQuestions {
			cat := bq.Category
			if _, err := s.store.CreateQuestion(ctx, issue.ID, bq.Text, &cat,
				"bank", nil, nextOrder+i); err != nil {
				s.logger.WarnContext(ctx, "Failed to add bank question on issue open",
					slog.Int64("bank_question_id", bq.ID),
					slog.String("error", err.Error()))
				continue
			}
			if err := s.store.MarkBankQuestionUsed(ctx, bq.ID); err != nil {
				s.logger.WarnContext(ctx, "Failed to mark bank question used",
					slog.Int64("bank_question_id", bq.ID),
					slog.String("error", err.Error()))
			}
		}
	}

	if err := s.store.SetIssueStatus(ctx, issue.ID, "collecting"); err != nil {
		return fmt.Errorf("opening issue %d for collecting: %w", issue.ID, err)
	}

	// Schedule reminder/auto-close events. Reminders are pinned to a
	// mid-morning local hour on their respective days.
	windowDuration := issue.Deadline.Sub(now)
	events := []struct {
		eventType   string
		scheduledAt time.Time
	}{
		{"reminder_1", atLocalHour(now.Add(windowDuration/2), reminderLocalHour, loc)},
		{"reminder_2", atLocalHour(issue.Deadline.AddDate(0, 0, -2), reminderLocalHour, loc)},
		{"auto_close", issue.Deadline},
	}
	for _, ev := range events {
		if err := s.store.CreateSchedulerEvent(ctx, &issue.ID, ev.eventType, ev.scheduledAt); err != nil {
			s.logger.WarnContext(ctx, "Failed to schedule event for opened issue",
				slog.Int64("issue_id", issue.ID),
				slog.String("event_type", ev.eventType),
				slog.String("error", err.Error()))
		}
	}

	// Send issue-open emails to every active member.
	questions, _ := s.store.ListQuestionsByIssue(ctx, issue.ID)

	users, err := s.store.ListActiveUsers(ctx)
	if err == nil {
		for i := range users {
			s.sendIssueOpenEmail(ctx, &users[i], issue, settings, questions)
		}
	}

	s.logger.InfoContext(ctx, "Issue opened for collecting",
		slog.Int64("issue_id", issue.ID),
		slog.Int("question_count", len(questions)),
		slog.Time("deadline", issue.Deadline),
	)

	return nil
}

// nextIssueOpensLabel returns a human-readable local timestamp for the queued
// create_next_issue event ("Monday, Jul 6 at 9:00 AM"), or "" when nothing is
// queued or auto-create is off. Lookup failures are logged and read as "not
// scheduled" — this only feeds informational UI copy.
func (s *Server) nextIssueOpensLabel(
	ctx context.Context, settings *store.Settings,
) string {
	if settings == nil || !settings.AutoCreateEnabled {
		return ""
	}

	ev, err := s.store.GetNextPendingEventByType(ctx, "create_next_issue")
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to look up next create_next_issue event",
			slog.String("error", err.Error()))
		return ""
	}
	if ev == nil {
		return ""
	}

	loc := s.settingsLocation(ctx, settings)

	return ev.ScheduledAt.In(loc).Format("Monday, Jan 2 at 3:04 PM")
}

// CreateNextIssue opens the next round when its scheduled time arrives.
// Called by the scheduler when a queued create_next_issue event fires.
//
// The normal path opens the draft that was pre-created at publish time
// (which may already hold question suggestions members sent while reading
// the current issue). When no draft exists — auto-create was enabled after
// the last publish, or the deployment predates pre-created drafts — a fresh
// issue is created on the spot. No-op if:
//   - auto_create_enabled is off (admin turned it off between queue + fire)
//   - a round is already collecting answers
func (s *Server) CreateNextIssue(ctx context.Context) error {
	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	if !settings.AutoCreateEnabled {
		s.logger.InfoContext(ctx, "create_next_issue skipped — auto-create disabled")
		return nil
	}

	collecting, err := s.store.HasCollectingIssue(ctx)
	if err != nil {
		return fmt.Errorf("checking collecting issue: %w", err)
	}

	if collecting {
		s.logger.InfoContext(ctx, "create_next_issue skipped — a round is already collecting")
		return nil
	}

	if draft, err := s.store.GetNextDraftIssue(ctx); err != nil {
		return fmt.Errorf("checking draft issue: %w", err)
	} else if draft != nil {
		return s.openIssueForCollecting(ctx, settings, draft, 0)
	}

	// Fallback: no pre-created draft — build one now and open it.
	now := time.Now().UTC()
	windowDays := settings.SubmissionWindowDays
	if windowDays < 3 {
		windowDays = 7
	}

	// All calendar math happens in the loop's timezone: the issue is labeled
	// with the local month (a 00:30 IST open must not become last month's
	// issue), and the deadline lands at a friendly local evening hour.
	loc := s.settingsLocation(ctx, settings)
	nowLocal := now.In(loc)
	deadline := atLocalHour(nowLocal.AddDate(0, 0, windowDays), deadlineLocalHour, loc)

	issueID, err := s.store.CreateIssue(ctx, nil,
		int(nowLocal.Month()), nowLocal.Year(), now, deadline,
	)
	if err != nil {
		return fmt.Errorf("creating issue: %w", err)
	}

	issue, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading created issue %d: %w", issueID, err)
	}

	return s.openIssueForCollecting(ctx, settings, issue, 0)
}

// nextIssueOpenTime calculates when the next issue should open based on the
// cadence and the current one's open time. Never returns a time in the past:
// if the cadence slot has already elapsed (e.g. publish ran very late), the
// next issue opens two days from now so members have a breather.
func nextIssueOpenTime(currentOpen time.Time, frequency string, now time.Time) time.Time {
	var next time.Time

	switch frequency {
	case "biweekly":
		next = currentOpen.AddDate(0, 0, 14)
	case "quarterly":
		next = currentOpen.AddDate(0, 3, 0)
	default: // monthly or unknown
		next = currentOpen.AddDate(0, 1, 0)
	}

	if next.Before(now.Add(24 * time.Hour)) {
		next = now.Add(48 * time.Hour)
	}

	return next
}

// SendCommentNotification delivers a "new comment on your response" email
// to the author of the response that was commented on. Fire-and-forget.
// Skipped if the author commented on their own response or has the
// comment_notify preference disabled.
func (s *Server) SendCommentNotification(
	ctx context.Context,
	responseAuthor *store.User, commenter *store.User,
	questionText, commentBody string, responseID int64,
) {
	if responseAuthor == nil || commenter == nil {
		return
	}

	if responseAuthor.ID == commenter.ID {
		return
	}

	prefs, err := s.store.GetNotificationPreferences(ctx, responseAuthor.ID)
	if err == nil && prefs != nil && !prefs.CommentNotify {
		return
	}

	settings, err := s.store.GetSettings(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings for comment notification",
			slog.String("error", err.Error()))
		return
	}

	raw, hash, err := auth.GenerateRandomToken(32)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to generate comment notification token",
			slog.String("error", err.Error()))
		return
	}

	expiresAt := time.Now().Add(30 * 24 * time.Hour)
	if err := s.store.CreateAuthToken(ctx, responseAuthor.ID, hash, "email_cta", expiresAt); err != nil {
		s.logger.ErrorContext(ctx, "Failed to create comment notification token",
			slog.String("error", err.Error()))
		return
	}

	authURL := fmt.Sprintf("%s/?auth=%s", s.config.BaseURL, raw)
	subject := fmt.Sprintf("%s commented on your response", commenter.Name)
	body := s.renderCommentEmail(
		settings.LoopName, responseAuthor.Name, commenter.Name,
		questionText, commentBody, authURL,
	)

	logID, _ := s.store.LogEmail(ctx, &responseAuthor.ID, nil, "comment_notification", "pending", nil)

	bgCtx := context.WithoutCancel(ctx)
	go func() {
		sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()

		if err := s.emailer.Send(sendCtx, responseAuthor.Email, subject, body); err != nil {
			errStr := err.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(bgCtx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(bgCtx, "Failed to send comment notification",
				slog.Int64("user_id", responseAuthor.ID),
				slog.String("error", errStr))
			return
		}

		now := time.Now()
		if logID > 0 {
			_ = s.store.UpdateEmailLog(bgCtx, logID, "sent", &now, nil)
		}
	}()
}

// renderReminderEmail builds the reminder email via reminder.html.
func (s *Server) renderReminderEmail(
	loopName, recipientName, month string,
	questions []string, authURL string,
	isFinal bool, daysLeft, responded, total int,
) string {
	return s.renderEmail("reminder.html", map[string]any{
		"LoopName":      loopName,
		"RecipientName": recipientName,
		"Month":         month,
		"Questions":     questions,
		"IsFinal":       isFinal,
		"DaysLeft":      daysLeft,
		"Responded":     responded,
		"Total":         total,
		"CTA": map[string]any{
			"URL":   authURL,
			"Label": "Share Your Answers",
		},
	})
}

// renderCommentEmail builds the comment notification email via comment.html.
func (s *Server) renderCommentEmail(
	loopName, recipientName, commenterName,
	questionText, commentBody, authURL string,
) string {
	excerpt := commentBody
	if len(excerpt) > 240 {
		excerpt = strings.TrimSpace(excerpt[:240]) + "…"
	}

	return s.renderEmail("comment.html", map[string]any{
		"LoopName":       loopName,
		"RecipientName":  recipientName,
		"CommenterName":  commenterName,
		"QuestionText":   questionText,
		"CommentExcerpt": excerpt,
		"CTA": map[string]any{
			"URL":   authURL,
			"Label": "View Thread",
		},
	})
}
