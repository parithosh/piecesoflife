package server

import (
	"context"
	"embed"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/email"
	"github.com/parithosh/piecesoflife/internal/store"
)

// integrationSessionSecret signs CSRF cookies in integration tests. It must
// match the secret used by csrfPair so minted tokens validate server-side.
const integrationSessionSecret = "integration-test-secret"

// integrationEnv bundles a fully-wired Server — including the complete
// middleware chain (recovery, security headers, logging, CSRF) — with its
// backing store for end-to-end HTTP tests against real routes and templates.
type integrationEnv struct {
	handler http.Handler
	store   *store.Store
	// srv exposes the wired Server for tests that drive scheduler actions
	// (CreateNextIssue etc.) directly rather than over HTTP.
	srv *Server
}

// newIntegrationEnv builds a Server backed by a fresh migrated SQLite store.
//
// DevMode is enabled so templates load from os.DirFS("templates") — embed.FS
// values cannot reference ../../templates from this package — which requires
// the process working directory to be the repo root for the test's duration
// (t.Chdir). DevMode also makes the email Sender a logging no-op, so no SMTP
// seam is needed. Tests using this env must not call t.Parallel (t.Chdir
// forbids it).
func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err, "resolve repo root")
	t.Chdir(root)

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	dbPath := filepath.Join(t.TempDir(), "integration.db")

	st, err := store.New(ctx, dbPath, logger)
	require.NoError(t, err, "open store")
	require.NoError(t, st.RunMigrations(ctx), "run migrations")
	require.NoError(t, st.SeedInstanceSettings(ctx), "seed instance settings")
	require.NoError(t, st.SeedDefaultGroup(ctx), "seed default group")
	require.NoError(t, st.CompleteSetup(ctx, 1), "complete group 1 setup")

	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		DevMode:       true,
		BaseURL:       "http://localhost:8080",
		DatabasePath:  dbPath,
		UploadPath:    t.TempDir(),
		SessionSecret: integrationSessionSecret,
	}

	// DevMode short-circuits Sender.Send before any SMTP dial, so the real
	// sender doubles as a no-op test double.
	emailer := email.NewSender(cfg, logger)

	srv := New(st, cfg, emailer, logger, embed.FS{}, embed.FS{})

	return &integrationEnv{handler: srv.Handler(), store: st, srv: srv}
}

// do runs a request through the full middleware chain and returns the recorder.
func (e *integrationEnv) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rr := httptest.NewRecorder()
	e.handler.ServeHTTP(rr, req)

	return rr
}

// createUser inserts an active member of group 1 directly through the store.
func (e *integrationEnv) createUser(t *testing.T, name, emailAddr string) *store.User {
	t.Helper()

	return e.createUserWithRole(t, name, emailAddr, "member")
}

// createUserWithRole inserts an active user with the given role in group 1.
func (e *integrationEnv) createUserWithRole(
	t *testing.T, name, emailAddr, role string,
) *store.User {
	t.Helper()
	ctx := context.Background()

	id, err := e.store.CreateUser(ctx, name, emailAddr)
	require.NoError(t, err, "create user")

	require.NoError(t, e.store.CreateMembership(ctx, 1, id, role), "create membership")

	user, err := e.store.GetUserByID(ctx, id)
	require.NoError(t, err, "load created user")

	return user
}

// sessionCookie mints a valid session for the user directly through the
// store and returns the cookie the browser would carry (raw token; the store
// keeps only the SHA-256 hash, mirroring createSessionAndSetCookie).
func (e *integrationEnv) sessionCookie(t *testing.T, userID int64) *http.Cookie {
	t.Helper()

	return e.sessionCookieForGroup(t, userID, 1)
}

// sessionCookieForGroup mints a session whose current Loop is groupID.
func (e *integrationEnv) sessionCookieForGroup(
	t *testing.T, userID, groupID int64,
) *http.Cookie {
	t.Helper()

	raw, hash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err, "generate session token")

	require.NoError(t, e.store.CreateSession(
		context.Background(), userID, hash, time.Now().Add(24*time.Hour), &groupID,
	), "create session")

	return &http.Cookie{Name: "session", Value: raw}
}

// seedIssue creates an issue in the given status with questionCount questions
// and returns the issue ID plus the question IDs in sort order.
func (e *integrationEnv) seedIssue(
	t *testing.T, status string, month, year, questionCount int,
) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	issueID, err := e.store.CreateIssue(ctx, 1, nil, month, year,
		now, now.Add(7*24*time.Hour))
	require.NoError(t, err, "create issue")

	if status == "published" {
		require.NoError(t, e.store.PublishIssue(ctx, issueID), "publish issue")
	} else {
		require.NoError(t, e.store.SetIssueStatus(ctx, issueID, status), "set issue status")
	}

	questionIDs := make([]int64, 0, questionCount)

	for i := 0; i < questionCount; i++ {
		qID, err := e.store.CreateQuestion(ctx, issueID,
			fmt.Sprintf("Integration question %d?", i+1), nil, "bank", nil, i)
		require.NoError(t, err, "create question")

		questionIDs = append(questionIDs, qID)
	}

	return issueID, questionIDs
}

// setPublicMementos toggles settings.AllowPublicMementos.
func (e *integrationEnv) setPublicMementos(t *testing.T, allowed bool) {
	t.Helper()
	ctx := context.Background()

	settings, err := e.store.GetSettings(ctx, 1)
	require.NoError(t, err, "load settings")

	settings.AllowPublicMementos = allowed
	require.NoError(t, e.store.UpdateSettings(ctx, settings), "update settings")
}

// csrfPair returns a matching signed CSRF cookie and header value, the way a
// browser echoes the double-submit cookie back in X-CSRF-Token.
func csrfPair() (*http.Cookie, string) {
	token := auth.GenerateSignedCSRFToken(integrationSessionSecret)
	return &http.Cookie{Name: "csrf_token", Value: token}, token
}

// newJSONRequest builds a request with a JSON body and content type.
func newJSONRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	return req
}

// findCookie returns the named Set-Cookie from a recorded response, or nil.
func findCookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}

	return nil
}
