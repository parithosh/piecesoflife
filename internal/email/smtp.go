package email

import (
	"context"
	"fmt"

	"github.com/parithosh/piecesoflife/internal/config"
	mail "github.com/wneessen/go-mail"
)

// smtpTransport delivers mail over classic SMTP submission (implicit TLS).
type smtpTransport struct {
	host     string
	port     int
	user     string
	pass     string
	from     string
	fromName string
}

func newSMTPTransport(cfg *config.Config) *smtpTransport {
	return &smtpTransport{
		host:     cfg.SMTPHost,
		port:     cfg.SMTPPort,
		user:     cfg.SMTPUser,
		pass:     cfg.SMTPPass,
		from:     cfg.FromEmail,
		fromName: cfg.FromName,
	}
}

func (t *smtpTransport) send(ctx context.Context, to, subject, htmlBody string) error {
	m := mail.NewMsg()

	if t.fromName != "" {
		if err := m.FromFormat(t.fromName, t.from); err != nil {
			return fmt.Errorf("setting from address: %w", err)
		}
	} else if err := m.From(t.from); err != nil {
		return fmt.Errorf("setting from address: %w", err)
	}

	if err := m.To(to); err != nil {
		return fmt.Errorf("setting to address: %w", err)
	}

	m.Subject(subject)
	m.SetBodyString(mail.TypeTextHTML, htmlBody)

	c, err := mail.NewClient(t.host,
		mail.WithPort(t.port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(t.user),
		mail.WithPassword(t.pass),
		mail.WithSSL(),
	)
	if err != nil {
		return fmt.Errorf("creating SMTP client: %w", err)
	}

	if err := c.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("sending email to %s: %w", to, err)
	}

	return nil
}
