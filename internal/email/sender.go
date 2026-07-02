// Package email provides email sending (SMTP or JMAP) and template rendering.
package email

import (
	"context"
	"log/slog"
	"regexp"
	"time"

	"github.com/parithosh/piecesoflife/internal/config"
)

// transport delivers a single rendered email. Implementations: smtpTransport
// (classic submission) and jmapTransport (Fastmail-style JMAP with an API
// token).
type transport interface {
	send(ctx context.Context, to, subject, htmlBody string) error
}

// Sender sends emails through the configured transport.
type Sender struct {
	transport transport
	provider  string
	baseURL   string
	devMode   bool
	logger    *slog.Logger
}

// BatchRecipient holds the data needed to send one email in a batch.
type BatchRecipient struct {
	UserID    int64
	Email     string
	Subject   string
	HTMLBody  string
	IssueID   *int64
	EmailType string
}

// NewSender creates a new email Sender using the transport selected by
// cfg.EmailProvider ("smtp" or "jmap").
func NewSender(cfg *config.Config, logger *slog.Logger) *Sender {
	log := logger.With(slog.String("component", "email"))

	var t transport
	if cfg.EmailProvider == "jmap" {
		t = newJMAPTransport(cfg, log)
	} else {
		t = newSMTPTransport(cfg)
	}

	return &Sender{
		transport: t,
		provider:  cfg.EmailProvider,
		baseURL:   cfg.BaseURL,
		devMode:   cfg.DevMode,
		logger:    log,
	}
}

// Send sends a single email with the given subject and HTML body.
//
// In dev mode the transport is skipped: the recipient, subject, and the
// hrefs found in the body are logged at info level and a nil error is
// returned. This lets you click through reminder, invite, and login links
// locally without standing up a mail account.
func (s *Sender) Send(ctx context.Context, to, subject, htmlBody string) error {
	if s.devMode {
		s.logger.InfoContext(ctx, "Email captured (dev mode — delivery skipped)",
			slog.String("to", to),
			slog.String("subject", subject),
			slog.Any("links", extractLinks(htmlBody)),
		)

		return nil
	}

	if err := s.transport.send(ctx, to, subject, htmlBody); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "Email sent",
		slog.String("provider", s.provider),
		slog.String("to", to),
		slog.String("subject", subject),
	)

	return nil
}

// BaseURL returns the configured base URL for building links.
func (s *Sender) BaseURL() string {
	return s.baseURL
}

// SendBatch sends emails to multiple recipients, logging successes and failures.
// The logFn callback is called for each email to record the result.
func (s *Sender) SendBatch(
	ctx context.Context, recipients []BatchRecipient,
	logFn func(ctx context.Context, r BatchRecipient, err error),
) {
	for _, r := range recipients {
		err := s.Send(ctx, r.Email, r.Subject, r.HTMLBody)
		if logFn != nil {
			logFn(ctx, r, err)
		}

		if err != nil {
			s.logger.WarnContext(ctx, "Batch email failed",
				slog.String("type", r.EmailType),
				slog.Int64("user_id", r.UserID),
				slog.String("error", err.Error()),
			)
		}

		// Small delay between sends to avoid rate limiting.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// hrefRe captures the URL inside a double-quoted href attribute. Email
// templates here always emit double-quoted hrefs, so we don't try to
// handle every HTML edge case — a missed link in dev mode is harmless.
var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

// extractLinks returns a deduplicated list of href URLs found in body.
// Used by Send when running in dev mode so the auth/reminder/login URLs
// surface in the logs in a copy-pasteable form.
func extractLinks(body string) []string {
	matches := hrefRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))

	for _, m := range matches {
		href := m[1]
		if _, dup := seen[href]; dup {
			continue
		}

		seen[href] = struct{}{}
		out = append(out, href)
	}

	return out
}
