package server

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

type contextKey string

const (
	userContextKey  contextKey = "user"
	groupContextKey contextKey = "group"
)

// GroupContext is the request's current Loop: the group row, its settings,
// and the user's membership in it. Membership is nil for instance admins
// operating on a Loop they don't belong to.
type GroupContext struct {
	Group      *store.Group
	Settings   *store.Settings
	Membership *store.Membership
	SessionID  int64
}

// UserFromContext extracts the authenticated user from a request context.
func UserFromContext(ctx context.Context) *store.User {
	u, _ := ctx.Value(userContextKey).(*store.User)
	return u
}

// GroupFromContext extracts the current Loop from a request context. Nil
// when the user has no Loops (possible for instance admins).
func GroupFromContext(ctx context.Context) *GroupContext {
	gc, _ := ctx.Value(groupContextKey).(*GroupContext)
	return gc
}

// currentGroupID returns the current Loop's ID, or 0 when none is resolved.
// Handlers behind requireGroupMiddleware can rely on a non-zero value.
func currentGroupID(ctx context.Context) int64 {
	if gc := GroupFromContext(ctx); gc != nil {
		return gc.Group.ID
	}

	return 0
}

// isGroupAdmin reports whether the request's user administers the current
// Loop — either through their membership role or as an instance admin.
func isGroupAdmin(ctx context.Context) bool {
	user := UserFromContext(ctx)
	if user == nil {
		return false
	}

	if user.IsInstanceAdmin {
		return true
	}

	gc := GroupFromContext(ctx)

	return gc != nil && gc.Membership != nil &&
		gc.Membership.IsActive && gc.Membership.Role == "admin"
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

		// Resolve the request's current Loop. An explicit ?g= (from email
		// links, which are otherwise ambiguous across Loops) wins over the
		// session's remembered Loop.
		gc := s.resolveCurrentGroup(ctx, user, session, r.URL.Query().Get("g"))
		if gc != nil {
			ctx = context.WithValue(ctx, groupContextKey, gc)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireGroupMiddleware gates Loop-scoped routes: the request must have a
// current Loop, and members can't use a Loop whose setup wizard hasn't
// finished (its admins can — they need the wizard and admin APIs).
func (s *Server) requireGroupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gc := GroupFromContext(r.Context())
		if gc == nil {
			user := UserFromContext(r.Context())
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusConflict,
					"no_group", "You are not a member of any Loop")
			} else if user != nil && user.IsInstanceAdmin {
				http.Redirect(w, r, "/instance", http.StatusSeeOther)
			} else {
				http.Redirect(w, r, "/loops", http.StatusSeeOther)
			}

			return
		}

		if !gc.Settings.SetupComplete && !isGroupAdmin(r.Context()) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusConflict,
					"setup_incomplete", "This Loop is still being set up")
			} else {
				http.Redirect(w, r, "/loops", http.StatusSeeOther)
			}

			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isGroupAdmin(r.Context()) {
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

// instanceAdminMiddleware gates the operator console and instance APIs.
func (s *Server) instanceAdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil || !user.IsInstanceAdmin {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusForbidden,
					"forbidden", "Instance admin access required")
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			}

			return
		}

		next.ServeHTTP(w, r)
	})
}

// resolveCurrentGroup picks the request's current Loop. Priority: an
// explicit ?g= override, the session's remembered Loop, the user's last
// Loop, their first membership, and (for instance admins only) the oldest
// active Loop on the instance. The chosen Loop is persisted back to the
// session and users.last_group_id when it changed. Returns nil when the
// user has no usable Loop.
func (s *Server) resolveCurrentGroup(
	ctx context.Context, user *store.User,
	session *store.Session, explicit string,
) *GroupContext {
	candidates := make([]int64, 0, 4)

	if explicit != "" {
		if id, err := strconv.ParseInt(explicit, 10, 64); err == nil {
			candidates = append(candidates, id)
		}
	}

	if session.GroupID != nil {
		candidates = append(candidates, *session.GroupID)
	}

	if user.LastGroupID != nil {
		candidates = append(candidates, *user.LastGroupID)
	}

	var gc *GroupContext

	for _, id := range candidates {
		if gc = s.tryGroup(ctx, user, id); gc != nil {
			break
		}
	}

	if gc == nil {
		if groups, err := s.store.ListUserGroups(ctx, user.ID); err == nil &&
			len(groups) > 0 {
			gc = s.tryGroup(ctx, user, groups[0].GroupID)
		}
	}

	if gc == nil && user.IsInstanceAdmin {
		if overviews, err := s.store.ListGroupOverviews(ctx); err == nil {
			for _, ov := range overviews {
				if !ov.IsActive {
					continue
				}

				if gc = s.tryGroup(ctx, user, ov.ID); gc != nil {
					break
				}
			}
		}
	}

	if gc == nil {
		return nil
	}

	gc.SessionID = session.ID

	if session.GroupID == nil || *session.GroupID != gc.Group.ID {
		if err := s.store.SetSessionGroup(ctx, session.ID, gc.Group.ID); err != nil {
			s.logger.ErrorContext(ctx, "Failed to persist session group",
				slog.String("error", err.Error()))
		}
	}

	if user.LastGroupID == nil || *user.LastGroupID != gc.Group.ID {
		if err := s.store.SetLastGroup(ctx, user.ID, gc.Group.ID); err != nil {
			s.logger.ErrorContext(ctx, "Failed to persist last group",
				slog.String("error", err.Error()))
		}
	}

	return gc
}

// tryGroup validates one candidate Loop for a user: the group must be
// active and the user must hold an active membership (instance admins may
// enter any active Loop). Returns nil when the candidate isn't usable.
func (s *Server) tryGroup(
	ctx context.Context, user *store.User, groupID int64,
) *GroupContext {
	group, err := s.store.GetGroup(ctx, groupID)
	if err != nil || !group.IsActive {
		return nil
	}

	membership, err := s.store.GetMembership(ctx, groupID, user.ID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.ErrorContext(ctx, "Failed to load membership",
				slog.Int64("group_id", groupID),
				slog.String("error", err.Error()))

			return nil
		}

		membership = nil
	}

	hasMembership := membership != nil && membership.IsActive
	if !hasMembership {
		if !user.IsInstanceAdmin {
			return nil
		}

		membership = nil
	}

	settings, err := s.store.GetSettings(ctx, groupID)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to load group settings",
			slog.Int64("group_id", groupID),
			slog.String("error", err.Error()))

		return nil
	}

	return &GroupContext{
		Group:      group,
		Settings:   settings,
		Membership: membership,
	}
}

// handleAuthParam exchanges an ?auth=TOKEN query parameter (the magic links
// embedded in emails) for a session and redirects to the same URL without
// the parameter. Two forgiving rules keep those links working in the real
// world:
//
//   - A visitor who already carries a valid session keeps it: the token is
//     ignored, not consumed, so a second click on an email button never
//     bounces a logged-in member to a login error.
//   - email_cta tokens are NOT single-use. Mail clients and corporate link
//     scanners prefetch URLs, and burning the token on that fetch would
//     break the member's real click. They stay valid until they expire.
//     Short-lived login tokens keep their single-use semantics.
func (s *Server) handleAuthParam(
	w http.ResponseWriter, r *http.Request, rawToken string,
) {
	redirectSansAuth := func() {
		q := r.URL.Query()
		q.Del("auth")
		r.URL.RawQuery = q.Encode()

		http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
	}

	if s.hasValidSession(r) {
		redirectSansAuth()
		return
	}

	tokenHash := hashSessionToken(rawToken)

	token, err := s.store.GetAuthTokenByHash(r.Context(), tokenHash)
	if err != nil {
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

	if token.Type != "email_cta" {
		if token.ConsumedAt != nil {
			http.Redirect(w, r, "/login?error=used", http.StatusSeeOther)
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
	}

	if err := s.createSessionAndSetCookie(
		w, r.Context(), token.UserID,
	); err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to create session",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	redirectSansAuth()
}

// hasValidSession reports whether the request carries a session cookie that
// maps to a live session for an active user.
func (s *Server) hasValidSession(r *http.Request) bool {
	cookie, err := r.Cookie("session")
	if err != nil {
		return false
	}

	session, err := s.store.GetSessionByHash(r.Context(), hashSessionToken(cookie.Value))
	if err != nil || session.ExpiresAt.Before(time.Now()) {
		return false
	}

	user, err := s.store.GetUserByID(r.Context(), session.UserID)

	return err == nil && user.IsActive
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

	// Seed the session's current Loop from the user's last one so the first
	// page after login lands where they left off; authMiddleware re-validates
	// and falls back if that Loop is gone.
	var groupID *int64
	if user, err := s.store.GetUserByID(ctx, userID); err == nil {
		groupID = user.LastGroupID
	}

	if err := s.store.CreateSession(ctx, userID, hash, expiresAt, groupID); err != nil {
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
