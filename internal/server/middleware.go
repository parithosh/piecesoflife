package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

type contextKey string

const userContextKey contextKey = "user"

// UserFromContext extracts the authenticated user from a request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey).(*store.User)
	return u
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.logger.ErrorContext(r.Context(), "Panic recovered",
					slog.Any("error", err),
					slog.String("stack", string(debug.Stack())),
				)
				http.Error(w, "Internal Server Error",
					http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	// CSP is narrow by default: same-origin for scripts, self + inline for
	// styles (template-local <style> blocks and Oat component styles).
	//
	// Exceptions:
	//   - img-src allows https: so admin-set avatar URLs (arbitrary CDNs)
	//     render without whitelisting each one.
	//   - frame-src allows a fixed allowlist of embed providers to match
	//     linkEmbed() in embed.go. Keep the two in sync.
	//   - script-src allows the inline theme-boot snippet in base.html via
	//     'unsafe-inline' — without it the FOUC avoidance would have to
	//     become an external file, which defeats the purpose.
	const embedHosts = "https://www.youtube-nocookie.com https://www.youtube.com " +
		"https://open.spotify.com https://embed.music.apple.com " +
		"https://w.soundcloud.com"

	csp := "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: https:; " +
		"media-src 'self'; " +
		"font-src 'self'; " +
		"frame-src 'self' " + embedHosts + "; " +
		"connect-src 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// X-Frame-Options kept DENY — we iframe *others*, nobody should
		// iframe *us*. CSP frame-ancestors would also work but XFO has
		// better legacy-browser support.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if !s.config.DevMode {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		var userID int64
		if u := UserFromContext(r.Context()); u != nil {
			userID = u.ID
		}

		s.logger.InfoContext(r.Context(), "Request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", ww.statusCode),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Int64("user_id", userID),
		)
	})
}

func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip CSRF for health checks, auth verification, and the
		// no-JavaScript login form (which cannot set the header). A forged
		// POST /login can only send a login email to the address owner —
		// the same effect any visitor gets via the page itself.
		if r.URL.Path == "/health" || r.URL.Path == "/auth/verify" ||
			r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}

		// Ensure a valid signed CSRF cookie exists. We re-mint when the
		// existing cookie is missing OR fails signature validation — the
		// latter shouldn't happen in normal use, but it self-heals if the
		// SESSION_SECRET is rotated or a malformed cookie is presented.
		cookie, err := r.Cookie("csrf_token")
		if err != nil || !auth.ValidateSignedCSRFToken(s.config.SessionSecret, cookie.Value) {
			token := auth.GenerateSignedCSRFToken(s.config.SessionSecret)
			http.SetCookie(w, &http.Cookie{
				Name:     "csrf_token",
				Value:    token,
				Path:     "/",
				HttpOnly: false,
				Secure:   !s.config.DevMode,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   86400 * 30,
			})

			cookie = &http.Cookie{Name: "csrf_token", Value: token}
		}

		// Validate CSRF on state-changing methods. Both the cookie and
		// header must match (double-submit) AND the cookie must carry a
		// valid HMAC signature derived from SESSION_SECRET — verified
		// above before we accept it as the cookie value.
		if r.Method == http.MethodPost ||
			r.Method == http.MethodPatch ||
			r.Method == http.MethodPut ||
			r.Method == http.MethodDelete {
			header := r.Header.Get("X-CSRF-Token")
			if header == "" || header != cookie.Value {
				writeError(w, http.StatusForbidden,
					"forbidden", "CSRF token mismatch")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handle email CTA auth tokens.
		if authToken := r.URL.Query().Get("auth"); authToken != "" {
			s.handleAuthParam(w, r, authToken)
			return
		}

		cookie, err := r.Cookie("session")
		if err != nil {
			s.respondUnauthorized(w, r)
			return
		}

		tokenHash := hashSessionToken(cookie.Value)

		session, err := s.store.GetSessionByHash(r.Context(), tokenHash)
		if err != nil || session.ExpiresAt.Before(time.Now()) {
			clearSessionCookie(w, s.config.DevMode)

			s.respondUnauthorized(w, r)

			return
		}

		user, err := s.store.GetUserByID(r.Context(), session.UserID)
		if err != nil || !user.IsActive {
			clearSessionCookie(w, s.config.DevMode)

			s.respondUnauthorized(w, r)

			return
		}

		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || user.Role != "admin" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusForbidden,
					"forbidden", "Admin access required")
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			}

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthParam(
	w http.ResponseWriter, r *http.Request, rawToken string,
) {
	tokenHash := hashSessionToken(rawToken)

	token, err := s.store.GetAuthTokenByHash(r.Context(), tokenHash)
	if err != nil || token.ConsumedAt != nil {
		http.Redirect(w, r, "/login?error=invalid", http.StatusSeeOther)
		return
	}

	if token.ExpiresAt.Before(time.Now()) {
		user, err := s.store.GetUserByID(r.Context(), token.UserID)
		if err != nil {
			http.Redirect(w, r, "/login?error=expired", http.StatusSeeOther)
			return
		}

		http.Redirect(w, r,
			"/login?email="+user.Email+"&expired=1", http.StatusSeeOther)

		return
	}

	if err := s.store.ConsumeAuthToken(r.Context(), token.ID); err != nil {
		if errors.Is(err, store.ErrTokenAlreadyConsumed) {
			http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
			return
		}

		s.logger.ErrorContext(r.Context(), "Failed to consume auth token",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	if err := s.createSessionAndSetCookie(
		w, r.Context(), token.UserID,
	); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create session",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	// Redirect to same URL without the auth parameter.
	q := r.URL.Query()
	q.Del("auth")
	r.URL.RawQuery = q.Encode()

	http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
}

func (s *Server) respondUnauthorized(
	w http.ResponseWriter, r *http.Request,
) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, http.StatusUnauthorized,
			"unauthorized", "Not logged in")
	} else {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (s *Server) createSessionAndSetCookie(
	w http.ResponseWriter, ctx context.Context, userID int64,
) error {
	raw, hash, err := auth.GenerateRandomToken(32)
	if err != nil {
		return err
	}

	expiresAt := time.Now().Add(30 * 24 * time.Hour)

	if err := s.store.CreateSession(ctx, userID, hash, expiresAt); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.config.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 30,
	})

	return nil
}

func hashSessionToken(raw string) string {
	return auth.SHA256Hex(raw)
}

func clearSessionCookie(w http.ResponseWriter, devMode bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !devMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
