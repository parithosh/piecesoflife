package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

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

// minSubmissionWindowDays and maxSubmissionWindowDays bound the answering
// window members get each round. Every validator, date-picker bound, and
// preview derives from these two numbers so the wizard, settings API,
// schedule editor, and dialog JS can never disagree about the allowed range.
const (
	minSubmissionWindowDays = 3
	maxSubmissionWindowDays = 21
)

// emailCTATokenTTL is how long the magic-link tokens embedded in outgoing
// emails stay valid.
const emailCTATokenTTL = 30 * 24 * time.Hour

// minReminderLead is the minimum head start a reminder needs to be worth
// queueing. It keeps short windows from firing a reminder right after the
// issue-open email, and late extensions from queueing already-past slots.
const minReminderLead = 12 * time.Hour

// scheduleIssueEvents queues a round's automated events: a reminder 3 days
// before the deadline, a final "last chance" reminder 1 day before it (both
// pinned to a mid-morning local hour and sent only to members who haven't
// answered yet), an admin_summary the day before publish (who's answered,
// who hasn't), and auto_close at the deadline.
//
// Email slots that are already past — or less than minReminderLead away —
// are skipped; the returned count says how many member reminders were
// actually queued, so callers can compensate (the extend handler queues an
// immediate nudge when none fit). Reminder/summary insert failures are
// logged and skipped; a failed auto_close insert is returned, because a
// round without one never closes or publishes.
func (s *Server) scheduleIssueEvents(
	ctx context.Context, settings *store.Settings, issueID int64,
	deadline time.Time,
) (remindersQueued int, err error) {
	events, _ := s.issueEventSpecs(ctx, settings, issueID, deadline)

	for _, ev := range events {
		if createErr := s.store.CreateSchedulerEvent(
			ctx, &issueID, ev.EventType, ev.ScheduledAt,
		); createErr != nil {
			if ev.EventType == "auto_close" {
				return remindersQueued, fmt.Errorf(
					"queueing auto_close for issue %d: %w", issueID, createErr)
			}

			s.logger.ErrorContext(ctx, "Failed to create scheduler event",
				slog.Int64("issue_id", issueID),
				slog.String("event_type", ev.EventType),
				slog.String("error", createErr.Error()))

			continue
		}

		if strings.HasPrefix(ev.EventType, "reminder") {
			remindersQueued++
		}
	}

	return remindersQueued, nil
}

// issueEventSpecs calculates a round's pending lifecycle without mutating
// the store. The schedule editor uses the same calculation inside one
// transaction with the deadline update; ordinary round opening inserts the
// returned rows directly through scheduleIssueEvents.
func (s *Server) issueEventSpecs(
	ctx context.Context, settings *store.Settings, issueID int64,
	deadline time.Time,
) ([]store.SchedulerEventSpec, int) {
	loc := s.settingsLocation(ctx, settings)
	now := time.Now()
	events := make([]store.SchedulerEventSpec, 0, 4)
	remindersQueued := 0

	for _, ev := range []struct {
		eventType   string
		scheduledAt time.Time
	}{
		{"reminder_1", atLocalHour(deadline.AddDate(0, 0, -3), reminderLocalHour, loc)},
		{"reminder_2", atLocalHour(deadline.AddDate(0, 0, -1), reminderLocalHour, loc)},
		{"admin_summary", atLocalHour(deadline.AddDate(0, 0, -1), reminderLocalHour, loc)},
		{"auto_close", deadline},
	} {
		if ev.eventType != "auto_close" && ev.scheduledAt.Before(now.Add(minReminderLead)) {
			s.logger.InfoContext(ctx, "Skipping scheduled email with too little lead time",
				slog.Int64("issue_id", issueID),
				slog.String("event_type", ev.eventType),
				slog.Time("scheduled_at", ev.scheduledAt))

			continue
		}

		events = append(events, store.SchedulerEventSpec{
			EventType: ev.eventType, ScheduledAt: ev.scheduledAt,
		})

		if strings.HasPrefix(ev.eventType, "reminder") {
			remindersQueued++
		}
	}

	return events, remindersQueued
}

// mintEmailCTAToken creates an "email_cta" auth token for userID and returns
// the raw token to embed in an email link.
func (s *Server) mintEmailCTAToken(ctx context.Context, userID int64) (string, error) {
	raw, hash, err := auth.GenerateRandomToken(32)
	if err != nil {
		return "", fmt.Errorf("generating email CTA token: %w", err)
	}

	expiresAt := time.Now().Add(emailCTATokenTTL)
	if err := s.store.CreateAuthToken(ctx, userID, hash, "email_cta", expiresAt); err != nil {
		return "", fmt.Errorf("storing email CTA token: %w", err)
	}

	return raw, nil
}

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

// effectiveWindowDays returns the configured submission window, falling back
// to a week when settings hold a nonsensical value (the wizard enforces the
// minimum).
func effectiveWindowDays(settings *store.Settings) int {
	if settings.SubmissionWindowDays < minSubmissionWindowDays {
		return 7
	}

	return settings.SubmissionWindowDays
}

// defaultDeadline derives a round's close from its open: the loop's
// answering window later, pinned to the friendly local evening hour. The
// single home of the open→deadline formula — every path that schedules a
// round derives its close here so the policy can't drift between them.
func defaultDeadline(
	open time.Time, settings *store.Settings, loc *time.Location,
) time.Time {
	return atLocalHour(
		open.In(loc).AddDate(0, 0, effectiveWindowDays(settings)),
		deadlineLocalHour, loc,
	)
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

	settings, err := s.store.GetSettings(ctx, issue.GroupID)
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

	nonResponders := make([]store.GroupMember, 0, len(progress.Members))
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

	monthStr := formatDate(issue.OpensAt)
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
				ctx, *schedulerEventID, user.ID, &issue.GroupID, &issueID, "reminder",
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
			logID, _ = s.store.LogEmail(ctx, &issue.GroupID, &user.ID, &issueID,
				"reminder", "pending", nil)
		}

		raw, err := s.mintEmailCTAToken(ctx, user.ID)
		if err != nil {
			errStr := err.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to mint reminder token",
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

// SendAdminSummaryForIssue emails every admin a pre-publish roster — who
// has answered and who hasn't — the day before the round closes, so they
// can nudge stragglers or extend the deadline while it still matters.
// Fired by the scheduler's admin_summary event; skips silently when the
// round is no longer collecting (published early, or the deadline moved).
func (s *Server) SendAdminSummaryForIssue(
	ctx context.Context, issueID int64, schedulerEventID *int64,
) error {
	issue, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading issue %d: %w", issueID, err)
	}

	if issue.Status != "collecting" {
		s.logger.InfoContext(ctx, "Admin summary skipped — issue is not collecting",
			slog.Int64("issue_id", issueID),
			slog.String("status", issue.Status))
		return nil
	}

	settings, err := s.store.GetSettings(ctx, issue.GroupID)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	progress, err := s.store.GetSubmissionProgress(ctx, issueID)
	if err != nil {
		return fmt.Errorf("loading progress for issue %d: %w", issueID, err)
	}

	responded := make([]string, 0, len(progress.Members))
	waiting := make([]string, 0, len(progress.Members))

	for _, mp := range progress.Members {
		if mp.Responded {
			responded = append(responded, mp.User.Name)
		} else {
			waiting = append(waiting, mp.User.Name)
		}
	}

	loc := s.settingsLocation(ctx, settings)
	monthStr := formatDate(issue.OpensAt)
	subject := fmt.Sprintf("%s — %s publishes tomorrow (%d/%d in)",
		settings.LoopName, monthStr, progress.Responded, progress.TotalMembers)

	members, err := s.store.ListActiveGroupMembers(ctx, issue.GroupID)
	if err != nil {
		return fmt.Errorf("listing members: %w", err)
	}

	for i := range members {
		admin := &members[i]
		if admin.Role != "admin" {
			continue
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		var logID int64
		if schedulerEventID != nil {
			var shouldSend bool
			logID, shouldSend, err = s.store.BeginSchedulerEmailAttempt(
				ctx, *schedulerEventID, admin.ID, &issue.GroupID, &issueID, "admin_summary",
			)
			if err != nil {
				s.logger.ErrorContext(ctx, "Failed to reserve admin summary email log",
					slog.Int64("user_id", admin.ID),
					slog.String("error", err.Error()))
				continue
			}
			if !shouldSend {
				continue
			}
		} else {
			logID, _ = s.store.LogEmail(ctx, &issue.GroupID, &admin.ID, &issueID,
				"admin_summary", "pending", nil)
		}

		raw, err := s.mintEmailCTAToken(ctx, admin.ID)
		if err != nil {
			errStr := err.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to mint admin summary token",
				slog.Int64("user_id", admin.ID),
				slog.String("error", err.Error()))
			continue
		}

		body := s.renderEmail("admin_summary.html", map[string]any{
			"LoopName":      settings.LoopName,
			"RecipientName": admin.Name,
			"Month":         monthStr,
			"DeadlineDay":   formatDay(issue.Deadline.In(loc)),
			"Responded":     responded,
			"Waiting":       waiting,
			"CTA": map[string]any{
				"URL": fmt.Sprintf("%s/admin?auth=%s&g=%d",
					s.config.BaseURL, raw, issue.GroupID),
				"Label": "Open the Loom",
			},
		})

		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sendErr := s.emailer.Send(sendCtx, admin.Email, subject, body)
		cancel()

		if sendErr != nil {
			errStr := sendErr.Error()
			if logID > 0 {
				_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
			}
			s.logger.ErrorContext(ctx, "Failed to send admin summary email",
				slog.Int64("user_id", admin.ID),
				slog.String("error", errStr))
			continue
		}

		now := time.Now()
		if logID > 0 {
			_ = s.store.UpdateEmailLog(ctx, logID, "sent", &now, nil)
		}
	}

	s.logger.InfoContext(ctx, "Admin summary sent",
		slog.Int64("issue_id", issueID),
		slog.Int("responded", progress.Responded),
		slog.Int("total", progress.TotalMembers),
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

	members, err := s.store.ListActiveGroupMembers(ctx, issue.GroupID)
	if err != nil {
		// Publish succeeded; notification failure shouldn't undo it.
		s.logger.ErrorContext(ctx, "Failed to list members for auto-publish notifications",
			slog.Int64("issue_id", issueID),
			slog.String("error", err.Error()))
		return nil
	}

	settings, err := s.store.GetSettings(ctx, issue.GroupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load settings for auto-publish",
			slog.String("error", err.Error()))
		return nil
	}

	updated, err := s.store.GetIssueByID(ctx, issueID)
	if err != nil {
		updated = issue
	}

	s.sendPublishNotifications(ctx, updated, members, settings)

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
	// An admin may already have pinned the next round from the dashboard's
	// schedule editor — a second create_next_issue event would open a
	// duplicate round later, so the pinned schedule wins. Only an event
	// whose round is still a draft counts: a pending event pointing at an
	// already-opened round is stale (e.g. the admin started the round
	// manually before the event fired) and must not block the next cycle.
	pending, err := s.store.GetPendingNextRoundEvent(ctx, settings.GroupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to check for queued next round — next round not scheduled",
			slog.Int64("group_id", settings.GroupID),
			slog.String("error", err.Error()))
		return
	}

	if pending != nil {
		s.logger.InfoContext(ctx, "Next round already queued — keeping its schedule",
			slog.Time("scheduled_at", pending.ScheduledAt))
		return
	}

	// With no genuine event queued, every remaining pending open event for
	// this Loop is stale by definition — left alone it would fire later and
	// open the round queued below off its date.
	if _, err := s.store.DeletePendingGroupEventsByType(ctx,
		settings.GroupID, "create_next_issue"); err != nil {
		s.logger.ErrorContext(ctx, "Failed to clear stale open events — next round not scheduled",
			slog.Int64("group_id", settings.GroupID),
			slog.String("error", err.Error()))
		return
	}

	loc := s.settingsLocation(ctx, settings)
	nextOpen := cadenceNextOpen(lastOpensAt, settings.Frequency, loc)

	// Ensure the next round exists as a draft first: the scheduler event
	// references it, and that reference is how the event knows which Loop
	// it belongs to. Both failure paths abort — creating a draft when the
	// existence check failed risks a duplicate round, and an event without
	// an issue reference cannot be attributed to this Loop at fire time.
	// The admin's "Start it now" button remains the manual recovery path.
	existing, err := s.store.GetNextDraftIssue(ctx, settings.GroupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to check for existing draft issue — next round not scheduled",
			slog.Int64("group_id", settings.GroupID),
			slog.String("error", err.Error()))
		return
	}

	var draftID int64

	if existing != nil {
		draftID = existing.ID
		// A draft whose open time is still comfortably ahead carries an
		// admin-chosen schedule (or survived an earlier queue failure) —
		// anchor the event to it instead of the cadence math.
		if existing.OpensAt.After(time.Now().Add(12 * time.Hour)) {
			nextOpen = existing.OpensAt
		} else {
			// The draft's own schedule is stale (its pin was consumed by a
			// no-op fire, or its open date slipped past). Re-anchor it to
			// the cadence slot: opened as-is it would carry a deadline
			// already in the past and auto-close an empty round minutes
			// after members are told it opened.
			openLocal := nextOpen.In(loc)
			deadline := defaultDeadline(nextOpen, settings, loc)
			month, year := s.issueLabel(ctx, settings.GroupID, existing.ID,
				int(openLocal.Month()), openLocal.Year())

			if err := s.store.UpdateIssueSchedule(ctx, existing.ID,
				month, year, nextOpen, deadline); err != nil {
				s.logger.ErrorContext(ctx, "Failed to re-anchor stale draft — next round not scheduled",
					slog.Int64("issue_id", existing.ID),
					slog.String("error", err.Error()))
				return
			}

			s.logger.InfoContext(ctx, "Re-anchored stale draft to the cadence slot",
				slog.Int64("issue_id", existing.ID),
				slog.Time("opens_at", nextOpen))
		}
	} else {
		openLocal := nextOpen.In(loc)
		deadline := defaultDeadline(nextOpen, settings, loc)
		month, year := s.issueLabel(ctx, settings.GroupID, 0,
			int(openLocal.Month()), openLocal.Year())

		draftID, err = s.store.CreateIssue(ctx, settings.GroupID, nil,
			month, year, nextOpen, deadline)
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to pre-create next draft issue — next round not scheduled",
				slog.Int64("group_id", settings.GroupID),
				slog.String("error", err.Error()))
			return
		}

		s.logger.InfoContext(ctx, "Pre-created next issue as draft",
			slog.Int64("issue_id", draftID),
			slog.Time("opens_at", nextOpen))
	}

	if err := s.store.CreateSchedulerEvent(ctx, &draftID, "create_next_issue", nextOpen); err != nil {
		s.logger.WarnContext(ctx, "Failed to queue create_next_issue event",
			slog.Time("scheduled_at", nextOpen),
			slog.String("error", err.Error()))
	} else {
		s.logger.InfoContext(ctx, "Queued create_next_issue event",
			slog.Time("scheduled_at", nextOpen),
		)
	}
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
	return s.openIssueForCollectingWithTransition(
		ctx, settings, issue, questionCount, nil, false,
	)
}

// openIssueForCollectingWithTransition allows the manual "start now" path
// to atomically reschedule the draft, consume its queued open event, and
// flip it to collecting. Questions are populated before that transition,
// so a failed transaction leaves a retryable scheduled draft.
func (s *Server) openIssueForCollectingWithTransition(
	ctx context.Context, settings *store.Settings, issue *store.Issue,
	questionCount int, transition func() error, eventsAlreadyScheduled bool,
) error {
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
		nextOrder += s.insertDefaultQuestions(ctx, issue.GroupID, issue.ID, nextOrder)
	}

	if need := questionTarget(settings, questionCount) - nextOrder; need > 0 {
		bankQuestions, err := s.store.SelectRandomUnusedQuestions(ctx, issue.GroupID, need)
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

	var openErr error
	if transition != nil {
		openErr = transition()
	} else {
		openErr = s.store.SetIssueStatus(ctx, issue.ID, "collecting")
	}

	if openErr != nil {
		return fmt.Errorf("opening issue %d for collecting: %w", issue.ID, openErr)
	}

	if !eventsAlreadyScheduled {
		// Queue reminders and auto-close anchored to the deadline.
		if _, evErr := s.scheduleIssueEvents(ctx, settings, issue.ID, issue.Deadline); evErr != nil {
			s.logger.ErrorContext(ctx, "Failed to schedule issue events — round will not auto-close; extend the deadline to requeue",
				slog.Int64("issue_id", issue.ID),
				slog.String("error", evErr.Error()))
		}
	}

	// Send issue-open emails to every active member.
	questions, _ := s.store.ListQuestionsByIssue(ctx, issue.ID)

	members, err := s.store.ListActiveGroupMembers(ctx, issue.GroupID)
	if err == nil {
		for i := range members {
			s.sendIssueOpenEmail(ctx, &members[i].User, issue, settings, questions)
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
// create_next_issue event ("Monday, 6 Jul at 9:00 AM"), or "" when nothing is
// queued or auto-create is off. Lookup failures are logged and read as "not
// scheduled" — this only feeds informational UI copy.
func (s *Server) nextIssueOpensLabel(
	ctx context.Context, settings *store.Settings,
) string {
	if settings == nil || !settings.AutoCreateEnabled {
		return ""
	}

	ev, err := s.store.GetPendingNextRoundEvent(ctx, settings.GroupID)
	if err != nil {
		s.logger.WarnContext(ctx, "Failed to look up next create_next_issue event",
			slog.String("error", err.Error()))
		return ""
	}

	if ev == nil {
		return ""
	}

	loc := s.settingsLocation(ctx, settings)

	return ev.ScheduledAt.In(loc).Format("Monday, 2 Jan at 3:04 PM")
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
func (s *Server) CreateNextIssue(
	ctx context.Context, groupID int64, scheduledAt time.Time,
) error {
	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	if !settings.AutoCreateEnabled {
		s.logger.InfoContext(ctx, "create_next_issue skipped — auto-create disabled")
		return nil
	}

	collecting, err := s.store.HasCollectingIssue(ctx, groupID)
	if err != nil {
		return fmt.Errorf("checking collecting issue: %w", err)
	}

	if collecting {
		s.logger.InfoContext(ctx, "create_next_issue skipped — a round is already collecting")
		return nil
	}

	// All calendar math happens in the loop's timezone: the issue is labeled
	// with the local month (a 00:30 IST open must not become last month's
	// issue), and the deadline lands at a friendly local evening hour.
	now := time.Now().UTC()
	loc := s.settingsLocation(ctx, settings)
	nowLocal := now.In(loc)

	if draft, err := s.store.GetNextDraftIssue(ctx, groupID); err != nil {
		return fmt.Errorf("checking draft issue: %w", err)
	} else if draft != nil {
		// The scheduler may have fetched an old event just before an admin
		// re-pinned this draft to a later date. Deleting that event from the
		// database cannot revoke the in-memory copy, so compare the event's
		// expected opening with the draft's current schedule before opening it.
		// The replacement event remains queued and will open it on the new date.
		if draft.OpensAt.After(scheduledAt) {
			s.logger.InfoContext(ctx, "create_next_issue skipped — draft was re-pinned after this event",
				slog.Int64("issue_id", draft.ID),
				slog.Time("event_scheduled_at", scheduledAt),
				slog.Time("current_opens_at", draft.OpensAt))

			return nil
		}

		// A very late fire (server down past the draft's own close) must
		// not open the round with a deadline already past or imminent —
		// the next tick would auto-close and publish it empty, minutes
		// after members are told it opened. Re-anchor to a fresh window
		// from now first; the same guard queueNextIssueEvent applies at
		// publish time. An error leaves the event unfired for a retry.
		if !draft.Deadline.After(now.Add(minReminderLead)) {
			deadline := defaultDeadline(now, settings, loc)
			month, year := s.issueLabel(ctx, groupID, draft.ID,
				int(nowLocal.Month()), nowLocal.Year())

			if err := s.store.UpdateIssueSchedule(ctx, draft.ID,
				month, year, now, deadline); err != nil {
				return fmt.Errorf("re-anchoring stale draft %d: %w", draft.ID, err)
			}

			draft.Month, draft.Year = month, year
			draft.OpensAt, draft.Deadline = now, deadline

			s.logger.InfoContext(ctx, "Re-anchored stale draft before opening",
				slog.Int64("issue_id", draft.ID),
				slog.Time("deadline", deadline))
		}

		return s.openIssueForCollecting(ctx, settings, draft, 0)
	}

	// Fallback: no pre-created draft — build one now and open it. The label
	// walks past (month, year) pairs other editions already claim, or the
	// archive could no longer address one of them.
	deadline := defaultDeadline(now, settings, loc)
	month, year := s.issueLabel(ctx, groupID, 0,
		int(nowLocal.Month()), nowLocal.Year())

	issueID, err := s.store.CreateIssue(ctx, groupID, nil,
		month, year, now, deadline,
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

// ReconcileAutoCreate re-queues the next-round event for any Loop whose
// auto-create cycle has stalled: auto-create on, no collecting round, and no
// pending create_next_issue event — including a draft whose open event was
// lost or whose open date slipped past. queueNextIssueEvent deliberately aborts
// (rather than queue a Loop-less event) when a transient store error hits
// at publish time — this reconciliation is the retry that makes that abort
// safe. Runs at scheduler startup and daily; per-Loop failures are logged
// so one bad Loop can't block the sweep.
func (s *Server) ReconcileAutoCreate(ctx context.Context) error {
	overviews, err := s.store.ListGroupOverviews(ctx)
	if err != nil {
		return fmt.Errorf("listing groups for auto-create reconcile: %w", err)
	}

	for _, ov := range overviews {
		if !ov.IsActive || !ov.SetupComplete {
			continue
		}

		settings, err := s.store.GetSettings(ctx, ov.ID)
		if err != nil {
			s.logger.ErrorContext(ctx, "Reconcile: failed to load settings",
				slog.Int64("group_id", ov.ID),
				slog.String("error", err.Error()))

			continue
		}

		if !settings.AutoCreateEnabled {
			continue
		}

		// Only a collecting round means the cycle is alive (its publish
		// re-queues the next one). HasActiveIssue would also count a
		// stalled draft — open date passed, no queued event — and skip
		// forever the exact state queueNextIssueEvent's re-anchor repairs.
		collecting, err := s.store.HasCollectingIssue(ctx, ov.ID)
		if err != nil || collecting {
			continue
		}

		// The same strict draft-joined check queueNextIssueEvent uses: a
		// stale event pointing at an already-opened round must not blind
		// the reconciler to a genuinely stalled Loop.
		pending, err := s.store.GetPendingNextRoundEvent(ctx, ov.ID)
		if err != nil || pending != nil {
			continue
		}

		// Stalled: nothing is open and nothing is queued. Anchor the next
		// round to the latest published edition; a Loop that never published
		// anything has nothing to continue from.
		published := "published"

		issues, err := s.store.ListIssues(ctx, ov.ID, &published)
		if err != nil || len(issues) == 0 {
			continue
		}

		s.logger.InfoContext(ctx, "Reconcile: re-queueing stalled auto-create cycle",
			slog.Int64("group_id", ov.ID))
		s.queueNextIssueEvent(ctx, settings, issues[0].OpensAt)
	}

	return nil
}

// cadenceNextOpen derives when the cadence would open the next round after
// one that opened at lastOpensAt: the frequency slot pinned to the friendly
// local morning hour, pushed a day when it lands under 12 hours away.
// Shared by publish-time queueing and the dashboard's schedule suggestion
// so the two can never drift apart.
func cadenceNextOpen(lastOpensAt time.Time, frequency string, loc *time.Location) time.Time {
	next := nextIssueOpenTime(lastOpensAt, frequency, time.Now().UTC())
	next = atLocalHour(next, nextOpenLocalHour, loc)

	if next.Before(time.Now().Add(12 * time.Hour)) {
		next = atLocalHour(next.AddDate(0, 0, 1), nextOpenLocalHour, loc)
	}

	return next
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
		next = addMonthsClamped(currentOpen, 3)
	default: // monthly or unknown
		next = addMonthsClamped(currentOpen, 1)
	}

	if next.Before(now.Add(24 * time.Hour)) {
		next = now.Add(48 * time.Hour)
	}

	return next
}

// addMonthsClamped advances by whole calendar months without Go's AddDate
// overflow behavior skipping short months (Jan 31 + one month otherwise
// normalizes into March). The target month's final day is used when needed.
func addMonthsClamped(t time.Time, months int) time.Time {
	target := time.Date(
		t.Year(), t.Month()+time.Month(months), 1,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location(),
	)
	lastDay := time.Date(
		target.Year(), target.Month()+1, 0,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location(),
	).Day()
	day := t.Day()
	if day > lastDay {
		day = lastDay
	}

	return time.Date(
		target.Year(), target.Month(), day,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location(),
	)
}

// SendCommentDigests drains the comment-notification queue into at most one
// email per recipient — a chatty newsletter must not mean an inbox full of
// per-comment pings. Fired daily by the comment_digest scheduler event.
// Rows for a recipient whose send fails stay queued and ride the next
// digest; recipients with comment_notify off are drained silently.
func (s *Server) SendCommentDigests(ctx context.Context) error {
	pending, err := s.store.ListPendingCommentNotifications(ctx)
	if err != nil {
		return fmt.Errorf("listing pending comment notifications: %w", err)
	}

	if len(pending) == 0 {
		return nil
	}

	// Rows arrive ordered by recipient — walk them in batches.
	sent := 0

	for start := 0; start < len(pending); {
		end := start
		for end < len(pending) &&
			pending[end].RecipientID == pending[start].RecipientID {
			end++
		}

		if s.sendCommentDigest(ctx, pending[start:end]) {
			sent++
		}

		start = end
	}

	s.logger.InfoContext(ctx, "Comment digests processed",
		slog.Int("pending_rows", len(pending)),
		slog.Int("digests_sent", sent))

	return nil
}

// sendCommentDigest builds and delivers one recipient's digest. Reports
// whether an email was actually handed to the mailer.
func (s *Server) sendCommentDigest(
	ctx context.Context, batch []store.PendingCommentNotification,
) bool {
	recipient := batch[0]

	ids := make([]int64, 0, len(batch))
	for _, p := range batch {
		ids = append(ids, p.NotificationID)
	}

	// A failed preference read deliberately falls through to sending:
	// missing one mute is recoverable (the email carries its own way out),
	// silently dropping a wanted digest is not.
	prefs, err := s.store.GetNotificationPreferences(ctx, recipient.RecipientID)
	if err == nil && prefs != nil && !prefs.CommentNotify {
		if err := s.store.DeleteCommentNotifications(ctx, ids); err != nil {
			s.logger.ErrorContext(ctx, "Failed to drain muted digest rows",
				slog.String("error", err.Error()))
		}

		return false
	}

	type digestItem struct {
		Commenter string
		Context   string
		Excerpt   string
	}

	items := make([]digestItem, 0, len(batch))

	var groupID *int64

	anyOwned := false

	for _, p := range batch {
		label, gid, owned := s.commentContextLabel(ctx, p)
		if groupID == nil && gid != 0 {
			g := gid
			groupID = &g
		}

		if owned {
			anyOwned = true
		}

		excerpt := p.Body
		if len(excerpt) > 240 {
			// Cut on a rune boundary — a byte slice mid-character would
			// put a � in the email.
			cut := 240
			for cut > 0 && !utf8.RuneStart(excerpt[cut]) {
				cut--
			}

			excerpt = strings.TrimSpace(excerpt[:cut]) + "…"
		}

		items = append(items, digestItem{
			Commenter: p.CommenterName,
			Context:   label,
			Excerpt:   excerpt,
		})
	}

	raw, err := s.mintEmailCTAToken(ctx, recipient.RecipientID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to mint digest token",
			slog.Int64("user_id", recipient.RecipientID),
			slog.String("error", err.Error()))

		return false // rows stay queued for the next digest
	}

	gParam := int64(1)
	if groupID != nil {
		gParam = *groupID
	}

	authURL := fmt.Sprintf("%s/?auth=%s&g=%d", s.config.BaseURL, raw, gParam)

	subject := fmt.Sprintf("%d new comments for you", len(items))
	if len(items) == 1 {
		if anyOwned {
			subject = fmt.Sprintf("%s commented on your piece", items[0].Commenter)
		} else {
			subject = fmt.Sprintf("%s replied in a thread you're in", items[0].Commenter)
		}
	}

	body := s.renderEmail("comment_digest.html", map[string]any{
		"RecipientName": recipient.RecipientName,
		"Count":         len(items),
		"AnyOwned":      anyOwned,
		"Items":         items,
		"CTA": map[string]any{
			"URL":   authURL,
			"Label": "Read & Reply",
		},
	})

	logID, _ := s.store.LogEmail(ctx, groupID, &recipient.RecipientID, nil,
		"comment_notification", "pending", nil)

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := s.emailer.Send(sendCtx, recipient.RecipientEmail, subject, body); err != nil {
		errStr := err.Error()
		if logID > 0 {
			_ = s.store.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
		}

		s.logger.ErrorContext(ctx, "Failed to send comment digest",
			slog.Int64("user_id", recipient.RecipientID),
			slog.String("error", errStr))

		return false // rows stay queued for the next digest
	}

	now := time.Now()
	if logID > 0 {
		_ = s.store.UpdateEmailLog(ctx, logID, "sent", &now, nil)
	}

	if err := s.store.DeleteCommentNotifications(ctx, ids); err != nil {
		s.logger.ErrorContext(ctx, "Failed to drain sent digest rows",
			slog.String("error", err.Error()))
	}

	return true
}

// commentContextLabel describes what a queued comment landed on — phrased
// for the recipient: "your answer to …" when they own the piece, "Meera's
// answer to …" when they're a thread participant on someone else's. Returns
// the Loop's ID for the sign-in link and whether the recipient owns the
// piece.
func (s *Server) commentContextLabel(
	ctx context.Context, p store.PendingCommentNotification,
) (label string, groupID int64, ownedByRecipient bool) {
	loopName := func(groupID int64) string {
		settings, err := s.store.GetSettings(ctx, groupID)
		if err != nil {
			return ""
		}

		return settings.LoopName
	}

	// possessive renders "your" for the recipient's own piece and
	// "<owner>'s" for anyone else's.
	possessive := func(ownerID int64) string {
		if ownerID == p.RecipientID {
			ownedByRecipient = true
			return "your"
		}

		owner, err := s.store.GetUserByID(ctx, ownerID)
		if err != nil {
			return "a friend's"
		}

		return owner.Name + "'s"
	}

	switch {
	case p.ResponseID != nil:
		issue, err := s.store.GetIssueByResponseID(ctx, *p.ResponseID)
		if err != nil {
			return "an answer", 0, false
		}

		label = "an answer"
		if resp, rErr := s.store.GetResponseByID(ctx, *p.ResponseID); rErr == nil {
			whose := possessive(resp.UserID)
			label = whose + " answer"
			if q, qErr := s.store.GetQuestionByID(ctx, resp.QuestionID); qErr == nil {
				label = fmt.Sprintf("%s answer to “%s”", whose, q.Text)
			}
		}

		if name := loopName(issue.GroupID); name != "" {
			label += " · " + name
		}

		return label, issue.GroupID, ownedByRecipient

	case p.DiaryDayID != nil:
		issue, err := s.store.GetIssueByDiaryDayID(ctx, *p.DiaryDayID)
		if err != nil {
			return "a notebook", 0, false
		}

		label = "a notebook"
		if day, dErr := s.store.GetDiaryDayByID(ctx, *p.DiaryDayID); dErr == nil {
			if section, sErr := s.store.GetDiarySectionByID(ctx, day.SectionID); sErr == nil {
				label = fmt.Sprintf("%s notebook — %s",
					possessive(section.UserID), rambleDayDisplay(day.Day))
			}
		}

		if name := loopName(issue.GroupID); name != "" {
			label += " · " + name
		}

		return label, issue.GroupID, ownedByRecipient

	case p.DumpItemID != nil:
		issue, err := s.store.GetIssueByDumpItemID(ctx, *p.DumpItemID)
		if err != nil {
			return "the photo & video dump", 0, false
		}

		label = "the photo & video dump"
		if item, iErr := s.store.GetDumpItemByID(ctx, *p.DumpItemID); iErr == nil {
			label = fmt.Sprintf("%s %s in the photo & video dump",
				possessive(item.UserID), item.Kind)
		}

		if name := loopName(issue.GroupID); name != "" {
			label += " · " + name
		}

		return label, issue.GroupID, ownedByRecipient
	}

	return "a piece", 0, false
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
