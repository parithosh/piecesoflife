package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

// ipRateLimiter is a small bounded sliding-window rate limiter keyed by client
// IP. It prevents a single source from spraying magic-link requests across
// many distinct email addresses (which would bypass the per-email counter).
type ipRateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string][]time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string][]time.Time),
	}
}

func (l *ipRateLimiter) allow(ip string) bool {
	if l == nil || ip == "" {
		return true
	}

	now := time.Now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	// Periodically sweep stale buckets so unbounded distinct IPs cannot grow
	// memory without bound.
	if len(l.entries) > 2048 {
		for k, times := range l.entries {
			kept := pruneTimes(times, cutoff)
			if len(kept) == 0 {
				delete(l.entries, k)
			} else {
				l.entries[k] = kept
			}
		}
	}

	kept := pruneTimes(l.entries[ip], cutoff)
	if len(kept) >= l.limit {
		l.entries[ip] = kept
		return false
	}

	l.entries[ip] = append(kept, now)
	return true
}

func pruneTimes(times []time.Time, cutoff time.Time) []time.Time {
	n := 0
	for _, t := range times {
		if t.After(cutoff) {
			times[n] = t
			n++
		}
	}
	return times[:n]
}

// clientIP extracts the network peer host from RemoteAddr. It intentionally
// does not trust unvalidated X-Forwarded-For headers from arbitrary clients.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

// magicLinkOutcome classifies a magic-link request so the JSON API and the
// no-JavaScript form fallback can share one flow.
type magicLinkOutcome int

const (
	magicLinkSent magicLinkOutcome = iota
	magicLinkInvalidEmail
	magicLinkRateLimited
)

// handleDevLogin mints a session for the user with the given email
// without requiring a magic link. Gated behind DEV_MODE — refuses to run
// in production. Used by the Playwright screenshot crawler.
// GET /dev/login?email=admin@example.com
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	if !s.config.DevMode {
		http.NotFound(w, r)
		return
	}

	email := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("email")))
	if email == "" {
		http.Error(w, "email query param required", http.StatusBadRequest)
		return
	}

	user, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		http.Error(w, "no user with that email", http.StatusNotFound)
		return
	}

	if !user.IsActive {
		http.Error(w, "user is deactivated", http.StatusForbidden)
		return
	}

	if err := s.createSessionAndSetCookie(w, r.Context(), user.ID); err != nil {
		s.logger.ErrorContext(r.Context(), "Dev login failed",
			slog.String("error", err.Error()))
		http.Error(w, "session creation failed", http.StatusInternalServerError)

		return
	}

	s.logger.InfoContext(r.Context(), "DEV session minted",
		slog.String("email", email),
		slog.Int64("user_id", user.ID),
	)

	redirect := r.URL.Query().Get("redirect")
	if redirect == "" || !strings.HasPrefix(redirect, "/") || strings.HasPrefix(redirect, "//") {
		redirect = "/"
	}

	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// handleLoginPage renders the login page.
// GET /login
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	data := LoginPageData{
		PageData: PageData{
			CSPNonce: cspNonceFromContext(r.Context()),
		},
		Error:   r.URL.Query().Get("error"),
		Success: r.URL.Query().Get("sent") == "1",
	}

	if r.URL.Query().Get("expired") == "1" {
		data.Error = "expired_resent"
	}

	// The login page is Loop-agnostic: brand it with the instance name and
	// the house look. It precedes knowing the circle, so no circle theme
	// (and never a circle photo) is shown here.
	name := "PiecesOfLife"
	if inst, err := s.store.GetInstanceSettings(r.Context()); err == nil {
		name = inst.InstanceName
	}

	data.Settings = &store.Settings{LoopName: name}

	s.renderPage(w, "login.html", data)
}

// handleRequestMagicLink processes a magic link login request.
// POST /api/auth/login
func (s *Server) handleRequestMagicLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}

	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}

	outcome, err := s.requestMagicLink(r.Context(), clientIP(r), req.Email)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Magic link request failed",
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "server_error", "Internal server error")
		return
	}

	switch outcome {
	case magicLinkInvalidEmail:
		writeError(w, http.StatusBadRequest, "validation_error", "Valid email is required")
	case magicLinkRateLimited:
		writeError(w, http.StatusTooManyRequests,
			"rate_limited", "Too many login attempts. Please wait and try again.")
	default:
		// Always 200 for sent/unknown/deactivated so the response never
		// reveals whether an email exists.
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Check your email for a login link",
		})
	}
}

// handleLoginForm is the no-JavaScript fallback for the login page.
// POST /login
func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=server", http.StatusSeeOther)
		return
	}

	outcome, err := s.requestMagicLink(r.Context(), clientIP(r), r.PostFormValue("email"))
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Magic link request failed",
			slog.String("error", err.Error()),
		)
		http.Redirect(w, r, "/login?error=server", http.StatusSeeOther)
		return
	}

	switch outcome {
	case magicLinkInvalidEmail:
		http.Redirect(w, r, "/login?error=email", http.StatusSeeOther)
	case magicLinkRateLimited:
		http.Redirect(w, r, "/login?error=rate_limited", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/login?sent=1", http.StatusSeeOther)
	}
}

// requestMagicLink runs the shared magic-link flow: validate the address,
// rate-limit, mint a token, and send the email in the background. Unknown and
// deactivated accounts return magicLinkSent so callers never reveal whether
// an email exists.
func (s *Server) requestMagicLink(ctx context.Context, remoteIP, email string) (magicLinkOutcome, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || !strings.Contains(email, "@") {
		return magicLinkInvalidEmail, nil
	}

	// Per-IP rate limit runs first so one caller cannot spray requests
	// across many distinct addresses to exhaust SMTP quota.
	if !s.loginIPLimiter.allow(remoteIP) {
		return magicLinkRateLimited, nil
	}

	// Rate limit: max 3 magic-link requests per address per hour. The
	// counter is keyed by a hash of the submitted address and runs BEFORE
	// the account lookup, so known, unknown, and deactivated addresses all
	// produce the same response sequence (3× sent, then rate_limited) —
	// the rate-limit boundary can't be used to enumerate accounts.
	emailHash := fmt.Sprintf("%x", sha256.Sum256([]byte(email)))

	attempts, err := s.store.CountRecentLoginAttempts(ctx, emailHash)
	if err != nil {
		return magicLinkSent, fmt.Errorf("counting login attempts: %w", err)
	}

	if attempts >= 3 {
		return magicLinkRateLimited, nil
	}

	if err := s.store.RecordLoginAttempt(ctx, emailHash); err != nil {
		return magicLinkSent, err // store already wraps with context
	}

	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		s.logger.InfoContext(ctx, "Login attempt for unknown email",
			slog.String("email", email),
		)

		return magicLinkSent, nil
	}

	if !user.IsActive {
		s.logger.InfoContext(ctx, "Login attempt for deactivated user",
			slog.Int64("user_id", user.ID),
		)

		return magicLinkSent, nil
	}

	if err := s.mintAndSendLoginLink(ctx, user); err != nil {
		return magicLinkSent, err
	}

	return magicLinkSent, nil
}

// mintAndSendLoginLink creates a 30-minute single-use login token for user
// and sends the magic-link email asynchronously.
func (s *Server) mintAndSendLoginLink(ctx context.Context, user *store.User) error {
	raw, hash, err := auth.GenerateRandomToken(32)
	if err != nil {
		return fmt.Errorf("generating login token: %w", err)
	}

	expiresAt := time.Now().Add(30 * time.Minute)

	if err := s.store.CreateAuthToken(ctx, user.ID, hash, "login", expiresAt); err != nil {
		return fmt.Errorf("creating auth token: %w", err)
	}

	link := s.config.BaseURL + "/auth/verify?token=" + raw
	subject := "Your login link"
	body := s.renderLoginEmail(user.Name, link)

	// In dev mode, print the magic link to stdout so it's usable without
	// real SMTP. Never enable in production — it leaks login tokens to logs.
	if s.config.DevMode {
		s.logger.InfoContext(ctx, "DEV magic link",
			slog.String("email", user.Email),
			slog.String("link", link),
		)
	}

	// Detach the context so the SMTP dial survives after the HTTP
	// handler returns. Preserve slog values for trace continuity.
	bgCtx := context.WithoutCancel(ctx)

	go func() {
		sendCtx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
		defer cancel()

		if err := s.emailer.Send(sendCtx, user.Email, subject, body); err != nil {
			s.logger.ErrorContext(sendCtx, "Failed to send login email",
				slog.Int64("user_id", user.ID),
				slog.String("error", err.Error()),
			)
		}
	}()

	return nil
}

// tryAutoResendLoginLink emails a fresh 30-minute login link to an active user
// when they click a used, revoked, or expired magic link while signed out.
// Shares the per-email 3/hr rate limit so stale links cannot be used to spam
// the user's inbox. Returns true when a fresh link was queued.
func (s *Server) tryAutoResendLoginLink(ctx context.Context, user *store.User) bool {
	if user == nil || !user.IsActive {
		return false
	}

	emailHash := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(user.Email)))))

	attempts, err := s.store.CountRecentLoginAttempts(ctx, emailHash)
	if err != nil || attempts >= 3 {
		return false
	}

	if err := s.store.RecordLoginAttempt(ctx, emailHash); err != nil {
		return false
	}

	if err := s.mintAndSendLoginLink(ctx, user); err != nil {
		s.logger.ErrorContext(ctx, "Failed to auto-resend login link",
			slog.Int64("user_id", user.ID),
			slog.String("error", err.Error()),
		)
		return false
	}

	return true
}

// handleVerifyToken processes a magic link token verification.
// GET /auth/verify
func (s *Server) handleVerifyToken(w http.ResponseWriter, r *http.Request) {
	// A GET that writes (consume token, mint session) — give it the same
	// deadline mutating methods get from mutationDeadlineMiddleware.
	ctx, cancel := context.WithTimeout(r.Context(), mutationDeadlineTimeout)
	defer cancel()

	r = r.WithContext(ctx)

	// If the visitor already has a valid session (e.g. clicking an old email
	// link again), let them straight in without burning the token or sending
	// an unnecessary email.
	if s.hasValidSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	rawToken := r.URL.Query().Get("token")
	if rawToken == "" {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	tokenHash := auth.SHA256Hex(rawToken)

	token, err := s.store.GetAuthTokenByHash(r.Context(), tokenHash)
	if err != nil {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	if token.Type != "login" {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	user, err := s.store.GetUserByID(r.Context(), token.UserID)
	if err != nil || !user.IsActive {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	if token.ConsumedAt != nil {
		if s.tryAutoResendLoginLink(r.Context(), user) {
			http.Redirect(w, r, "/login?error=used_resent", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
		}
		return
	}

	if token.ExpiresAt.Before(time.Now()) {
		// Consume the expired token to prevent reuse. A losing race here is
		// harmless — the token is expired either way.
		if err := s.store.ConsumeAuthToken(r.Context(), token.ID); err != nil &&
			!errors.Is(err, store.ErrTokenAlreadyConsumed) {
			s.logger.ErrorContext(r.Context(), "Failed to consume expired token",
				slog.String("error", err.Error()),
			)
		}

		if s.tryAutoResendLoginLink(r.Context(), user) {
			http.Redirect(w, r, "/login?expired=1", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/login?error=expired", http.StatusSeeOther)
		}
		return
	}

	// Consume the token atomically. If another tab/click already consumed it,
	// send a fresh login link instead of minting a second session.
	if err := s.store.ConsumeAuthToken(r.Context(), token.ID); err != nil {
		if errors.Is(err, store.ErrTokenAlreadyConsumed) {
			if s.tryAutoResendLoginLink(r.Context(), user) {
				http.Redirect(w, r, "/login?error=used_resent", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
			}
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to consume token",
			slog.String("error", err.Error()),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Create a session.
	if err := s.createSessionAndSetCookie(w, r.Context(), token.UserID); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create session",
			slog.String("error", err.Error()),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout destroys the current session and revokes any outstanding
// unconsumed magic-link / email CTA tokens for the user.
// POST /api/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session")
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	tokenHash := hashSessionToken(cookie.Value)

	session, err := s.store.GetSessionByHash(r.Context(), tokenHash)
	if err == nil {
		if delErr := s.store.DeleteSession(r.Context(), session.ID); delErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to delete session",
				slog.String("error", delErr.Error()),
			)
		}
		if revErr := s.store.RevokeAuthTokensForUser(r.Context(), session.UserID); revErr != nil {
			s.logger.ErrorContext(r.Context(), "Failed to revoke auth tokens on logout",
				slog.Int64("user_id", session.UserID),
				slog.String("error", revErr.Error()),
			)
		}
	}

	clearSessionCookie(w, s.config.DevMode)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the current authenticated user as JSON.
// GET /api/auth/me
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
		return
	}

	prefs, err := s.store.GetNotificationPreferences(r.Context(), user.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logger.ErrorContext(r.Context(), "Failed to get notification preferences",
			slog.String("error", err.Error()),
		)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user":        user,
		"preferences": prefs,
	})
}

// renderLoginEmail builds the login magic link email. html/template performs
// auto-escaping on all fields.
func (s *Server) renderLoginEmail(name, link string) string {
	if name == "" {
		name = "there"
	}

	loopName := s.loopNameForEmail()

	return s.renderEmail("login.html", map[string]any{
		"LoopName": loopName,
		"Name":     name,
		"CTA": map[string]any{
			"URL":   link,
			"Label": "Log In",
		},
	})
}

// loopNameForEmail returns the instance name for Loop-agnostic emails
// (login links), falling back to a default if it can't be loaded.
func (s *Server) loopNameForEmail() string {
	inst, err := s.store.GetInstanceSettings(context.Background())
	if err != nil || inst == nil || inst.InstanceName == "" {
		return "PiecesOfLife"
	}

	return inst.InstanceName
}
