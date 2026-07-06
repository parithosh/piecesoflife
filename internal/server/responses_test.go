package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/config"
	"github.com/parithosh/piecesoflife/internal/store"
)

func newResponseTestServer(t *testing.T) (*Server, *store.Store, *store.User) {
	t.Helper()

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbPath := filepath.Join(t.TempDir(), "test.db")

	st, err := store.New(ctx, dbPath, logger)
	require.NoError(t, err)
	require.NoError(t, st.RunMigrations(ctx))

	t.Cleanup(func() { _ = st.Close() })

	require.NoError(t, st.SeedDefaultGroup(ctx))

	userID, err := st.CreateUser(ctx, "friend", "friend@example.com")
	require.NoError(t, err)
	require.NoError(t, st.CreateMembership(ctx, 1, userID, "member"))

	user, err := st.GetUserByID(ctx, userID)
	require.NoError(t, err)

	srv := &Server{
		store:  st,
		config: &config.Config{UploadPath: t.TempDir(), SessionSecret: "test-secret"},
		logger: logger,
	}

	return srv, st, user
}

func addIssueWithQuestion(
	t *testing.T, st *store.Store, status string,
) (int64, int64) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	issueID, err := st.CreateIssue(ctx, 1, nil, int(now.Month()), now.Year(),
		now, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, st.SetIssueStatus(ctx, issueID, status))

	questionID, err := st.CreateQuestion(ctx, issueID, "What happened this week?",
		nil, "bank", nil, 0)
	require.NoError(t, err)

	return issueID, questionID
}

func requestAsUser(
	t *testing.T, method, target, body string, user *store.User,
) *http.Request {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))

	ctx := context.WithValue(req.Context(), userContextKey, user)
	// Handlers called directly (no middleware) still need the current-Loop
	// context; group 1 is the test fixture's Loop.
	ctx = context.WithValue(ctx, groupContextKey, &GroupContext{
		Group: &store.Group{ID: 1, IsActive: true},
	})
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	return req
}

func TestHandleCreateResponseRejectsNonCollectingIssue(t *testing.T) {
	srv, st, user := newResponseTestServer(t)
	_, questionID := addIssueWithQuestion(t, st, "published")

	req := requestAsUser(t, http.MethodPost, "/api/responses",
		`{"question_id":`+strconv.FormatInt(questionID, 10)+`}`, user)
	rr := httptest.NewRecorder()

	srv.handleCreateResponse(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "not_collecting")
}

func TestHandleAutosaveAllowsSubmittedResponseWhileCollecting(t *testing.T) {
	srv, st, user := newResponseTestServer(t)
	_, questionID := addIssueWithQuestion(t, st, "collecting")

	responseID, err := st.CreateResponse(context.Background(), user.ID, questionID)
	require.NoError(t, err)
	require.NoError(t, st.SubmitResponse(context.Background(), responseID))

	req := requestAsUser(t, http.MethodPut, "/api/responses/"+strconv.FormatInt(responseID, 10)+"/autosave",
		`{"version":1,"blocks":[{"type":"text","content":"edited after submit"}]}`, user)
	req.SetPathValue("id", strconv.FormatInt(responseID, 10))
	rr := httptest.NewRecorder()

	srv.handleAutosave(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)

	blocks, err := st.ListBlocksByResponse(context.Background(), responseID)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Content)
	assert.Equal(t, "edited after submit", *blocks[0].Content)
}

func TestHandleAutosaveRejectsPublishedIssue(t *testing.T) {
	srv, st, user := newResponseTestServer(t)
	issueID, questionID := addIssueWithQuestion(t, st, "collecting")

	responseID, err := st.CreateResponse(context.Background(), user.ID, questionID)
	require.NoError(t, err)
	require.NoError(t, st.PublishIssue(context.Background(), issueID))

	req := requestAsUser(t, http.MethodPut, "/api/responses/"+strconv.FormatInt(responseID, 10)+"/autosave",
		`{"version":1,"blocks":[{"type":"text","content":"too late"}]}`, user)
	req.SetPathValue("id", strconv.FormatInt(responseID, 10))
	rr := httptest.NewRecorder()

	srv.handleAutosave(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "not_collecting")
}
