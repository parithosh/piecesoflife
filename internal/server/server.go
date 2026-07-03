// Package server provides the HTTP server, route registration, and handler helpers.
package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/email"
	"github.com/parithosh/piecesoflife/internal/store"
)

// Server is the main HTTP server that handles all web and API requests.
type Server struct {
	store   *store.Store
	config  *config.Config
	emailer *email.Sender
	// tmplMu guards pages/emails. In DevMode they are rebuilt on every
	// render, so reads and the rebuild swap must not race — otherwise a
	// concurrent request can observe a half-populated map and 500 with
	// "Template not found".
	tmplMu sync.RWMutex
	pages  map[string]*template.Template // page templates, keyed by filename
	emails map[string]*template.Template // email templates, keyed by filename
	mux    *http.ServeMux
	logger *slog.Logger

	// Embedded filesystems, injected from main.go where go:embed is valid.
	staticFS    embed.FS
	templatesFS embed.FS
}

// ErrorResponse is the standard JSON error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides structured error information.
type ErrorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// ListResponse is a generic paginated response envelope.
type ListResponse[T any] struct {
	Items   []T `json:"items"`
	Total   int `json:"total"`
	Page    int `json:"page"`
	PerPage int `json:"per_page"`
}

const maxJSONBodyBytes = 4 << 20

// assetVersion busts the browser and service-worker caches for /static/
// assets. It prefers the VCS revision baked into the binary; without one it
// falls back to process start time, so every deploy (or restart) serves
// freshly-versioned asset URLs.
var assetVersion = computeAssetVersion()

func computeAssetVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 8 {
				return setting.Value[:8]
			}
		}
	}

	return strconv.FormatInt(time.Now().Unix(), 36)
}

// Pagination holds parsed pagination parameters.
type Pagination struct {
	Page    int
	PerPage int
	Offset  int
}

// PageData is the base data passed to all page templates.
type PageData struct {
	User      *store.User
	Settings  *store.Settings
	CSRFToken string
}

// LoginPageData is the template data for the login page.
type LoginPageData struct {
	PageData
	Error   string
	Email   string
	Success bool
}

// New creates a new Server with all dependencies wired.
// The staticFS and templatesFS are embedded filesystems containing the static
// assets and HTML templates respectively, injected from main.go.
func New(
	st *store.Store, cfg *config.Config,
	emailer *email.Sender, logger *slog.Logger,
	staticFS, templatesFS embed.FS,
) *Server {
	s := &Server{
		store:       st,
		config:      cfg,
		emailer:     emailer,
		mux:         http.NewServeMux(),
		logger:      logger.With(slog.String("component", "server")),
		staticFS:    staticFS,
		templatesFS: templatesFS,
	}

	s.loadTemplates()
	s.registerRoutes()

	return s
}

// Handler returns the fully-wrapped HTTP handler with all middleware applied.
func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux

	// Apply middleware in reverse order (outermost executes first).
	handler = s.csrfMiddleware(handler)
	handler = s.loggingMiddleware(handler)
	handler = s.securityHeadersMiddleware(handler)
	handler = s.recoveryMiddleware(handler)

	return handler
}

func (s *Server) registerRoutes() {
	// Static files.
	var sFS fs.FS

	if s.config.DevMode {
		sFS = os.DirFS("static")
	} else {
		sub, err := fs.Sub(s.staticFS, "static")
		if err != nil {
			s.logger.Error("Failed to create static sub-filesystem",
				slog.String("error", err.Error()))
			sFS = s.staticFS
		} else {
			sFS = sub
		}
	}

	s.mux.Handle("GET /static/",
		http.StripPrefix("/static/", http.FileServer(http.FS(sFS))),
	)

	// Public routes (no auth).
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLoginForm)
	s.mux.HandleFunc("GET /auth/verify", s.handleVerifyToken)
	s.mux.HandleFunc("POST /api/auth/login", s.handleRequestMagicLink)

	// Dev-only: mint a session by email without magic link. Gated by
	// DEV_MODE inside the handler. Used by the screenshot crawler.
	s.mux.HandleFunc("GET /dev/login", s.handleDevLogin)

	// Mementos — optionally public (checked inside the handler).
	s.mux.HandleFunc("GET /m/{id}", s.handleMemento)
	s.mux.HandleFunc("GET /m/{id}/file/{path...}", s.handleMementoFile)

	// Landing page — handles auth check and routing internally.
	s.mux.HandleFunc("GET /{$}", s.handleLanding)

	// Middleware helpers.
	authMW := func(h http.HandlerFunc) http.Handler {
		return s.authMiddleware(http.HandlerFunc(h))
	}
	adminMW := func(h http.HandlerFunc) http.Handler {
		return s.authMiddleware(s.adminMiddleware(http.HandlerFunc(h)))
	}

	// Auth API routes.
	s.mux.Handle("POST /api/auth/logout", authMW(s.handleLogout))
	s.mux.Handle("GET /api/auth/me", authMW(s.handleMe))

	// Authenticated page routes.
	s.mux.Handle("GET /issues", authMW(s.handleIssueArchive))
	s.mux.Handle("GET /issues/{year}/{month}", authMW(s.handleIssuePage))
	s.mux.Handle("GET /issues/{id}/respond", authMW(s.handleRespondPage))
	s.mux.Handle("GET /albums", authMW(s.handleAlbumsPage))
	s.mux.Handle("GET /profile", authMW(s.handleProfilePage))

	// Admin page routes.
	s.mux.Handle("GET /admin", adminMW(s.handleAdminDashboard))
	s.mux.Handle("GET /admin/members", adminMW(s.handleAdminMembers))
	s.mux.Handle("GET /admin/members/{userId}/submission",
		adminMW(s.handleAdminMemberSubmission))
	s.mux.Handle("GET /admin/questions", adminMW(s.handleAdminQuestions))
	s.mux.Handle("GET /admin/settings", adminMW(s.handleAdminSettings))
	s.mux.Handle("GET /admin/setup", adminMW(s.handleAdminSetup))

	// Authenticated file serving.
	s.mux.Handle("GET /uploads/", authMW(s.handleUploadServe))

	// User API routes (auth required).
	s.mux.Handle("GET /api/users", authMW(s.handleListUsers))
	s.mux.Handle("PATCH /api/users/{id}", authMW(s.handleUpdateUser))
	s.mux.Handle("GET /api/users/{id}/preferences", authMW(s.handleGetPreferences))
	s.mux.Handle("PATCH /api/users/{id}/preferences", authMW(s.handleUpdatePreferences))

	// Admin-only API routes.
	s.mux.Handle("POST /api/users/invite", adminMW(s.handleInviteUser))
	s.mux.Handle("POST /api/onboarding/complete", adminMW(s.handleCompleteOnboarding))
	s.mux.Handle("GET /api/admin/settings", adminMW(s.handleGetSettings))
	s.mux.Handle("PATCH /api/admin/settings", adminMW(s.handleUpdateSettings))
	s.mux.Handle("GET /api/admin/email-log", adminMW(s.handleEmailLog))
	s.mux.Handle("POST /api/admin/email/test", adminMW(s.handleSendTestEmail))
	s.mux.Handle("POST /api/admin/resend/{logId}", adminMW(s.handleResendEmail))
	s.mux.Handle("POST /api/admin/send-reminder/{issueId}", adminMW(s.handleSendReminder))
	s.mux.Handle("GET /api/admin/export", adminMW(s.handleExport))
	s.mux.Handle("GET /api/admin/export.zip", adminMW(s.handleExportZip))
	s.mux.Handle("GET /api/default-questions", adminMW(s.handleListDefaultQuestions))
	s.mux.Handle("PATCH /api/default-questions", adminMW(s.handleUpdateAllDefaultQuestions))
	s.mux.Handle("PATCH /api/default-questions/{id}", adminMW(s.handleUpdateDefaultQuestion))
	s.mux.Handle("GET /api/question-bank", adminMW(s.handleListQuestionBank))
	s.mux.Handle("POST /api/question-bank", adminMW(s.handleCreateBankQuestion))
	s.mux.Handle("PATCH /api/question-bank/{id}", adminMW(s.handleEditBankQuestion))
	s.mux.Handle("DELETE /api/question-bank/{id}", adminMW(s.handleDeleteBankQuestion))

	// Issue API routes.
	s.mux.Handle("GET /api/issues", authMW(s.handleListIssues))
	s.mux.Handle("GET /api/issues/{id}", authMW(s.handleGetIssue))
	s.mux.Handle("POST /api/issues", adminMW(s.handleCreateIssue))
	s.mux.Handle("PATCH /api/issues/{id}", adminMW(s.handleUpdateIssue))
	s.mux.Handle("POST /api/issues/{id}/publish", adminMW(s.handlePublishIssue))
	s.mux.Handle("POST /api/issues/{id}/extend", adminMW(s.handleExtendDeadline))
	s.mux.Handle("GET /api/issues/{id}/questions", authMW(s.handleListQuestions))
	s.mux.Handle("GET /api/issues/{id}/progress", authMW(s.handleGetProgress))
	s.mux.Handle("POST /api/issues/{id}/count-admin", adminMW(s.handleSetCountAdmin))
	s.mux.Handle("GET /api/issues/{id}/responses", authMW(s.handleListResponses))
	s.mux.Handle("GET /api/issues/{id}/responses/mine", authMW(s.handleListMyResponses))
	s.mux.Handle("POST /api/issues/{id}/questions", adminMW(s.handleAddQuestion))
	s.mux.Handle("PATCH /api/questions/{id}", adminMW(s.handleEditQuestion))
	s.mux.Handle("DELETE /api/questions/{id}", adminMW(s.handleDeleteQuestion))
	s.mux.Handle("POST /api/questions/submit", authMW(s.handleFriendSubmitQuestion))

	// Response API routes.
	s.mux.Handle("POST /api/responses", authMW(s.handleCreateResponse))
	s.mux.Handle("DELETE /api/responses/{id}", authMW(s.handleDeleteResponse))
	s.mux.Handle("POST /api/responses/{id}/submit", authMW(s.handleSubmitResponse))
	s.mux.Handle("GET /api/responses/{id}/blocks", authMW(s.handleListBlocks))
	s.mux.Handle("POST /api/responses/{id}/blocks", authMW(s.handleAddBlock))
	s.mux.Handle("PATCH /api/blocks/{id}", authMW(s.handleUpdateBlock))
	s.mux.Handle("DELETE /api/blocks/{id}", authMW(s.handleDeleteBlock))
	s.mux.Handle("POST /api/responses/{id}/blocks/reorder", authMW(s.handleReorderBlocks))
	s.mux.Handle("POST /api/responses/{id}/blocks/upload", authMW(s.handleUploadPhoto))
	s.mux.Handle("POST /api/issues/{id}/dump", authMW(s.handleDumpUpload))
	s.mux.Handle("DELETE /api/dump/{id}", authMW(s.handleDumpDelete))
	s.mux.Handle("PUT /api/responses/{id}/autosave", authMW(s.handleAutosave))

	// Comment routes.
	s.mux.Handle("GET /api/responses/{id}/comments", authMW(s.handleListComments))
	s.mux.Handle("POST /api/responses/{id}/comments", authMW(s.handleAddComment))
	s.mux.Handle("DELETE /api/comments/{id}", authMW(s.handleDeleteComment))

	// Albums API.
	s.mux.Handle("GET /api/albums", authMW(s.handleListAlbums))
}

func (s *Server) loadTemplates() {
	funcMap := template.FuncMap{
		"formatDate":     formatDate,
		"formatDateTime": formatDateTime,
		"formatRelative": formatRelative,
		"truncate":       truncate,
		"safeHTML":       safeHTML,
		"uploadURL":      s.uploadURL,
		"mementoFileURL": s.mementoFileURL,
		"add":            func(a, b int) int { return a + b },
		"sub":            func(a, b int) int { return a - b },
		"mul":            func(a, b int) int { return a * b },
		"percent": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a * 100 / b
		},
		"seq": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i
			}
			return s
		},
		"contains":      strings.Contains,
		"hasPrefix":     strings.HasPrefix,
		"lower":         strings.ToLower,
		"upper":         strings.ToUpper,
		"join":          strings.Join,
		"letterAvatar":  letterAvatar,
		"questionWord":  questionWord,
		"dropCap":       dropCap,
		"jsonMarshal":   jsonMarshal,
		"categoryLabel": categoryLabel,
		"dict":          dict,
		"linkEmbed":     linkEmbed,
		"assetVersion":  func() string { return assetVersion },
	}

	var tmplFS fs.FS
	if s.config.DevMode {
		tmplFS = os.DirFS("templates")
	} else {
		sub, err := fs.Sub(s.templatesFS, "templates")
		if err != nil {
			s.logger.Error("Failed to create templates sub-filesystem",
				slog.String("error", err.Error()))
			return
		}

		tmplFS = sub
	}

	// Parse each page template individually with the layout so
	// {{define "content"}} blocks don't overwrite each other. Build into
	// local maps and publish them with a single locked swap so in-flight
	// renders never see a partially-populated map.
	layoutFiles := []string{"layout/base.html"}
	pagePatterns := []string{"page/*.html", "page/admin/*.html"}

	pages := make(map[string]*template.Template, 16)

	for _, pattern := range pagePatterns {
		matches, err := fs.Glob(tmplFS, pattern)
		if err != nil {
			s.logger.Error("Failed to glob page templates",
				slog.String("pattern", pattern),
				slog.String("error", err.Error()))
			continue
		}

		for _, match := range matches {
			// Template name is the base filename (e.g., "login.html").
			name := match[strings.LastIndex(match, "/")+1:]
			files := append(layoutFiles, match)

			t, err := template.New("").Funcs(funcMap).ParseFS(tmplFS, files...)
			if err != nil {
				s.logger.Error("Failed to parse page template",
					slog.String("file", match),
					slog.String("error", err.Error()))
				continue
			}

			pages[name] = t
		}
	}

	// Parse email templates, one per child file paired with base.html. Same
	// rationale as pages: every child defines {{block "email_content"}}, so
	// they must be parsed in separate template sets to avoid overriding
	// each other.
	emails := make(map[string]*template.Template, 8)

	if emailMatches, err := fs.Glob(tmplFS, "email/*.html"); err != nil {
		s.logger.Error("Failed to glob email templates",
			slog.String("error", err.Error()))
	} else {
		for _, match := range emailMatches {
			name := match[strings.LastIndex(match, "/")+1:]
			if name == "base.html" {
				continue
			}

			t, err := template.New("").Funcs(funcMap).ParseFS(tmplFS,
				"email/base.html", match,
			)
			if err != nil {
				s.logger.Error("Failed to parse email template",
					slog.String("file", match),
					slog.String("error", err.Error()))
				continue
			}

			emails[name] = t
		}
	}

	s.tmplMu.Lock()
	s.pages = pages
	s.emails = emails
	s.tmplMu.Unlock()

	s.logger.Info("Loaded templates",
		slog.Int("pages", len(pages)),
		slog.Int("emails", len(emails)))
}

// renderEmail executes the named email template and returns the rendered
// HTML body. The template name is the filename (e.g., "invite.html").
// Fails closed: returns an empty string and logs on any error, so callers
// don't accidentally send a broken email.
func (s *Server) renderEmail(name string, data any) string {
	if s.config.DevMode {
		s.loadTemplates()
	}

	s.tmplMu.RLock()
	t, ok := s.emails[name]
	s.tmplMu.RUnlock()

	if !ok || t == nil {
		s.logger.Error("Email template not found",
			slog.String("template", name))
		return ""
	}

	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "email_base", data); err != nil {
		s.logger.Error("Email template render failed",
			slog.String("template", name),
			slog.String("error", err.Error()))
		return ""
	}

	return buf.String()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable,
			"unhealthy", "Database unreachable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	setupComplete, err := s.store.IsSetupComplete(r.Context())
	if err != nil {
		s.logger.ErrorContext(r.Context(), "Failed to check setup status",
			slog.String("error", err.Error()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)

		return
	}

	if !setupComplete {
		http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
		return
	}

	// Invite / email-CTA links land here as /?auth=TOKEN. The landing
	// page is registered without authMiddleware so we must consume the
	// token here ourselves; handleAuthParam exchanges it for a session
	// cookie and redirects back to / without the query param, at which
	// point the cookie check below succeeds.
	if authToken := r.URL.Query().Get("auth"); authToken != "" {
		s.handleAuthParam(w, r, authToken)
		return
	}

	// Check if user has a valid session.
	cookie, err := r.Cookie("session")
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	tokenHash := hashSessionToken(cookie.Value)

	session, err := s.store.GetSessionByHash(r.Context(), tokenHash)
	if err != nil || session.ExpiresAt.Before(time.Now()) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	user, err := s.store.GetUserByID(r.Context(), session.UserID)
	if err != nil || !user.IsActive {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Authenticated user: redirect based on role.
	if user.Role == "admin" {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	} else {
		if activeIssue, activeErr := s.store.GetActiveIssue(r.Context()); activeErr == nil &&
			activeIssue.Status == "collecting" {
			http.Redirect(w, r,
				fmt.Sprintf("/issues/%d/respond", activeIssue.ID),
				http.StatusSeeOther)
			return
		}

		http.Redirect(w, r, "/issues", http.StatusSeeOther)
	}
}

func (s *Server) renderPage(
	w http.ResponseWriter, tmplName string, data any,
) {
	if s.config.DevMode {
		s.loadTemplates()
	}

	s.tmplMu.RLock()
	t, ok := s.pages[tmplName]
	s.tmplMu.RUnlock()

	if !ok || t == nil {
		s.logger.Error("Template not found", slog.String("template", tmplName))
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", data); err != nil {
		s.logger.Error("Template render failed",
			slog.String("template", tmplName),
			slog.String("error", err.Error()),
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("Failed to encode JSON response",
			slog.String("error", err.Error()))
	}
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Error: ErrorDetail{Code: code, Message: message},
	})
}

// writeValidationError writes a JSON validation error response.
func writeValidationError(w http.ResponseWriter, fields map[string]string) {
	writeJSON(w, http.StatusBadRequest, ErrorResponse{
		Error: ErrorDetail{
			Code:    "validation_error",
			Message: "Invalid input",
			Fields:  fields,
		},
	})
}

// readJSON decodes a JSON request body into the given value.
func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	return dec.Decode(v)
}

// parsePagination extracts pagination parameters from query string.
func parsePagination(r *http.Request) Pagination {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))

	if page < 1 {
		page = 1
	}

	if perPage < 1 {
		perPage = 50
	}

	if perPage > 100 {
		perPage = 100
	}

	return Pagination{
		Page:    page,
		PerPage: perPage,
		Offset:  (page - 1) * perPage,
	}
}

func formatDate(t time.Time) string {
	return t.Format("January 2006")
}

func formatDateTime(t time.Time) string {
	return t.Format("Jan 2, 2006 at 3:04 PM")
}

func formatRelative(t time.Time) string {
	now := time.Now()
	if t.After(now) {
		return formatFutureRelative(t.Sub(now))
	}

	return formatPastRelative(now.Sub(t))
}

func formatFutureRelative(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "soon"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "in 1 minute"
		}

		return fmt.Sprintf("in %d minutes", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "in 1 hour"
		}

		return fmt.Sprintf("in %d hours", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "in 1 day"
		}

		return fmt.Sprintf("in %d days", days)
	}
}

func formatPastRelative(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}

		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}

		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}

		return fmt.Sprintf("%d days ago", days)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + "..."
}

func safeHTML(s string) template.HTML {
	return template.HTML(s) //nolint:gosec // intentional for rendered content
}

func letterAvatar(name string) string {
	if len(name) == 0 {
		return "?"
	}

	return strings.ToUpper(name[:1])
}

func questionWord(n int) string {
	words := []string{
		"Zero", "One", "Two", "Three", "Four", "Five",
		"Six", "Seven", "Eight", "Nine", "Ten", "Eleven",
		"Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen",
		"Seventeen", "Eighteen", "Nineteen", "Twenty",
	}
	if n >= 0 && n < len(words) {
		return strings.ToUpper(words[n])
	}

	return strconv.Itoa(n)
}

// dropCapMinRunes is the shortest leading text block that can carry a
// magazine-style drop cap without the oversized initial dwarfing the
// answer (e.g. a bare "hey!").
const dropCapMinRunes = 100

// dropCap reports whether the first answer on a paginated spread should
// render a drop cap. It only applies when the leading block is text long
// enough for the copy to wrap alongside the initial.
func dropCap(blocks []store.ResponseBlock) bool {
	if len(blocks) == 0 || blocks[0].Type != "text" || blocks[0].Content == nil {
		return false
	}

	return utf8.RuneCountInString(strings.TrimSpace(*blocks[0].Content)) >= dropCapMinRunes
}

func jsonMarshal(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b) //nolint:gosec // intentional for template data
}

func categoryLabel(cat string) string {
	labels := map[string]string{
		"life_updates":    "Life Updates",
		"deep_thoughts":   "Deep Thoughts",
		"fun_silly":       "Fun & Silly",
		"memories":        "Memories",
		"goals":           "Goals",
		"recommendations": "Recommendations",
		"hypotheticals":   "Hypotheticals",
	}

	if l, ok := labels[cat]; ok {
		return l
	}

	return cat
}

func dict(pairs ...any) map[string]any {
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs)-1; i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			continue
		}

		m[key] = pairs[i+1]
	}

	return m
}
