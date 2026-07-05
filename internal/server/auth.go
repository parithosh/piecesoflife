package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

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
		Error:   r.URL.Query().Get("error"),
		Email:   r.URL.Query().Get("email"),
		Success: r.URL.Query().Get("sent") == "1",
	}

	if r.URL.Query().Get("expired") == "1" {
		data.Error = "expired"
	}

	// The login page is Loop-agnostic: brand it with the instance name.
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

	outcome, err := s.requestMagicLink(r.Context(), req.Email)
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

	outcome, err := s.requestMagicLink(r.Context(), r.PostFormValue("email"))
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
func (s *Server) requestMagicLink(ctx context.Context, email string) (magicLinkOutcome, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" || !strings.Contains(email, "@") {
		return magicLinkInvalidEmail, nil
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
		return magicLinkSent, fmt.Errorf("recording login attempt: %w", err)
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

	raw, hash, err := auth.GenerateRandomToken(32)
	if err != nil {
		return magicLinkSent, fmt.Errorf("generating login token: %w", err)
	}

	expiresAt := time.Now().Add(30 * time.Minute)

	if err := s.store.CreateAuthToken(ctx, user.ID, hash, "login", expiresAt); err != nil {
		return magicLinkSent, fmt.Errorf("creating auth token: %w", err)
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

	return magicLinkSent, nil
}

// handleVerifyToken processes a magic link token verification.
// GET /auth/verify
func (s *Server) handleVerifyToken(w http.ResponseWriter, r *http.Request) {
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

	if token.ConsumedAt != nil {
		http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
		return
	}

	if token.Type != "login" {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	if token.ExpiresAt.Before(time.Now()) {
		// Token expired — auto-send a fresh one.
		user, err := s.store.GetUserByID(r.Context(), token.UserID)
		if err != nil {
			http.Redirect(w, r, "/login?error=expired", http.StatusSeeOther)
			return
		}

		// Consume the expired token to prevent reuse. A losing race here is
		// harmless — the token is expired either way.
		if err := s.store.ConsumeAuthToken(r.Context(), token.ID); err != nil &&
			!errors.Is(err, store.ErrTokenAlreadyConsumed) {
			s.logger.ErrorContext(r.Context(), "Failed to consume expired token",
				slog.String("error", err.Error()),
			)
		}

		// Generate a fresh login token.
		newRaw, newHash, err := auth.GenerateRandomToken(32)
		if err == nil {
			newExpiry := time.Now().Add(30 * time.Minute)
			if err := s.store.CreateAuthToken(r.Context(), user.ID, newHash, "login", newExpiry); err == nil {
				link := s.config.BaseURL + "/auth/verify?token=" + newRaw
				body := s.renderLoginEmail(user.Name, link)

				bgCtx := context.WithoutCancel(r.Context())
				go func() {
					ctx, cancel := context.WithTimeout(bgCtx, 30*time.Second)
					defer cancel()

					_ = s.emailer.Send(ctx, user.Email, "Your login link", body)
				}()
			}
		}

		http.Redirect(w, r,
			"/login?email="+user.Email+"&expired=1", http.StatusSeeOther)
		return
	}

	// Consume the token atomically. If another tab/click already consumed it,
	// redirect to login rather than minting a second session.
	if err := s.store.ConsumeAuthToken(r.Context(), token.ID); err != nil {
		if errors.Is(err, store.ErrTokenAlreadyConsumed) {
			http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
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

// handleLogout destroys the current session.
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
