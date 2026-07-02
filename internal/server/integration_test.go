package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/auth"
)

// TestAuthVerifyMagicLinkFlow walks a login token through its full lifecycle:
// first use mints a session and redirects home, second use bounces to
// error=used because consumption is atomic.
func TestAuthVerifyMagicLinkFlow(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Alice", "alice@example.com")

	raw, hash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAuthToken(
		ctx, user.ID, hash, "login", time.Now().Add(30*time.Minute)))

	// First use: session cookie set, redirected to /.
	rr := env.do(t, httptest.NewRequest(http.MethodGet, "/auth/verify?token="+raw, nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	assert.Equal(t, "/", rr.Header().Get("Location"))

	session := findCookie(rr, "session")
	require.NotNil(t, session, "expected a session cookie to be set")
	assert.NotEmpty(t, session.Value)

	// The minted session works against an authed endpoint.
	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meReq.AddCookie(session)

	meRR := env.do(t, meReq)
	assert.Equal(t, http.StatusOK, meRR.Code)
	assert.Contains(t, meRR.Body.String(), "alice@example.com")

	// Second use of the same token: already consumed.
	rr2 := env.do(t, httptest.NewRequest(http.MethodGet, "/auth/verify?token="+raw, nil))
	require.Equal(t, http.StatusSeeOther, rr2.Code)
	assert.Equal(t, "/login?error=used", rr2.Header().Get("Location"))
	assert.Nil(t, findCookie(rr2, "session"), "consumed token must not mint a session")
}

// TestAuthVerifyRejectsBadTokens covers expired, wrong-type, and unknown
// tokens presented to GET /auth/verify.
func TestAuthVerifyRejectsBadTokens(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Bob", "bob@example.com")

	tests := []struct {
		name         string
		createToken  bool
		tokenType    string
		expiresIn    time.Duration
		wantContains string
	}{
		{
			name:         "expired login token redirects with expired flag",
			createToken:  true,
			tokenType:    "login",
			expiresIn:    -time.Minute,
			wantContains: "expired=1",
		},
		{
			name:         "email_cta token is invalid at verify",
			createToken:  true,
			tokenType:    "email_cta",
			expiresIn:    30 * time.Minute,
			wantContains: "/login?error=invalid",
		},
		{
			name:         "unknown token is invalid",
			createToken:  false,
			wantContains: "/login?error=invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, hash, err := auth.GenerateRandomToken(32)
			require.NoError(t, err)

			if tt.createToken {
				require.NoError(t, env.store.CreateAuthToken(
					ctx, user.ID, hash, tt.tokenType, time.Now().Add(tt.expiresIn)))
			}

			rr := env.do(t, httptest.NewRequest(
				http.MethodGet, "/auth/verify?token="+raw, nil))

			require.Equal(t, http.StatusSeeOther, rr.Code)
			assert.Contains(t, rr.Header().Get("Location"), tt.wantContains)
			assert.Nil(t, findCookie(rr, "session"),
				"rejected token must not mint a session")
		})
	}
}

// TestCSRFEnforcement verifies the double-submit + HMAC-signature CSRF check
// on a mutating API route, running through the full middleware chain.
func TestCSRFEnforcement(t *testing.T) {
	env := newIntegrationEnv(t)

	user := env.createUser(t, "Carol", "carol@example.com")
	session := env.sessionCookie(t, user.ID)
	_, questionIDs := env.seedIssue(t, "collecting", 5, 2026, 1)

	body := fmt.Sprintf(`{"question_id":%d}`, questionIDs[0])

	cookie, header := csrfPair()
	otherToken := auth.GenerateSignedCSRFToken(integrationSessionSecret)

	tests := []struct {
		name       string
		cookie     *http.Cookie
		header     string
		wantStatus int
	}{
		{
			name:       "missing header is rejected",
			cookie:     cookie,
			header:     "",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "header not matching cookie is rejected",
			cookie:     cookie,
			header:     otherToken,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "header without cookie is rejected",
			cookie:     nil,
			header:     header,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "matching signed pair is accepted",
			cookie:     cookie,
			header:     header,
			wantStatus: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newJSONRequest(http.MethodPost, "/api/responses", body)
			req.AddCookie(session)

			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}

			if tt.header != "" {
				req.Header.Set("X-CSRF-Token", tt.header)
			}

			rr := env.do(t, req)

			assert.Equal(t, tt.wantStatus, rr.Code, "body: %s", rr.Body.String())

			if tt.wantStatus == http.StatusForbidden {
				assert.Contains(t, rr.Body.String(), "CSRF token mismatch")
			}
		})
	}
}

// TestLoginFormNoJS exercises the form-encoded POST /login fallback, which is
// exempt from CSRF and must never reveal whether an email address exists.
func TestLoginFormNoJS(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	// A user with 3 recent login tokens is over the per-hour rate limit.
	limited := env.createUser(t, "Dave", "dave@example.com")
	for i := 0; i < 3; i++ {
		_, hash, err := auth.GenerateRandomToken(32)
		require.NoError(t, err)
		require.NoError(t, env.store.CreateAuthToken(
			ctx, limited.ID, hash, "login", time.Now().Add(30*time.Minute)))
	}

	tests := []struct {
		name         string
		email        string
		wantLocation string
	}{
		{
			name:         "malformed email",
			email:        "invalid",
			wantLocation: "/login?error=email",
		},
		{
			name:         "unknown email never reveals existence",
			email:        "ghost@example.com",
			wantLocation: "/login?sent=1",
		},
		{
			name:         "rate limited after three recent tokens",
			email:        "dave@example.com",
			wantLocation: "/login?error=rate_limited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			form := url.Values{"email": {tt.email}}
			req := httptest.NewRequest(http.MethodPost, "/login",
				strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			rr := env.do(t, req)

			require.Equal(t, http.StatusSeeOther, rr.Code)
			assert.Equal(t, tt.wantLocation, rr.Header().Get("Location"))
		})
	}
}

// TestAutosaveVersionConflict verifies optimistic concurrency on
// PUT /api/responses/{id}/autosave through the full stack: a stale version
// gets 409 with the current version in the body; the correct version saves
// and increments.
func TestAutosaveVersionConflict(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Erin", "erin@example.com")
	session := env.sessionCookie(t, user.ID)
	_, questionIDs := env.seedIssue(t, "collecting", 5, 2026, 1)

	responseID, err := env.store.CreateResponse(ctx, user.ID, questionIDs[0])
	require.NoError(t, err)

	resp, err := env.store.GetResponseByID(ctx, responseID)
	require.NoError(t, err)

	csrfCookie, csrfHeader := csrfPair()
	target := fmt.Sprintf("/api/responses/%d/autosave", responseID)

	autosave := func(version int, content string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(
			`{"version":%d,"blocks":[{"type":"text","content":%q,"sort_order":0}]}`,
			version, content)

		req := newJSONRequest(http.MethodPut, target, body)
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)

		return env.do(t, req)
	}

	// Stale version → 409 with the current version echoed back.
	rr := autosave(resp.Version+7, "stale write")
	require.Equal(t, http.StatusConflict, rr.Code, "body: %s", rr.Body.String())

	var conflict struct {
		Error          string `json:"error"`
		CurrentVersion int    `json:"current_version"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &conflict))
	assert.Equal(t, "version_conflict", conflict.Error)
	assert.Equal(t, resp.Version, conflict.CurrentVersion)

	// Correct version → 200 and the version increments.
	rr = autosave(resp.Version, "fresh write")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var saved struct {
		Version int `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &saved))
	assert.Equal(t, resp.Version+1, saved.Version)

	updated, err := env.store.GetResponseByID(ctx, responseID)
	require.NoError(t, err)
	assert.Equal(t, resp.Version+1, updated.Version)

	blocks, err := env.store.ListBlocksByResponse(ctx, responseID)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].Content)
	assert.Equal(t, "fresh write", *blocks[0].Content)
}

// TestMementoAccessControl covers /m/{id} visibility: anonymous viewers are
// redirected to /login unless AllowPublicMementos is on; members always see
// published mementos; unknown IDs 404.
func TestMementoAccessControl(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	author := env.createUser(t, "Frank", "frank@example.com")
	session := env.sessionCookie(t, author.ID)

	// A memento requires a submitted (non-draft) response on a published
	// issue; responses can only be created while collecting.
	issueID, questionIDs := env.seedIssue(t, "collecting", 5, 2026, 1)

	responseID, err := env.store.CreateResponse(ctx, author.ID, questionIDs[0])
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, responseID))
	require.NoError(t, env.store.PublishIssue(ctx, issueID))

	tests := []struct {
		name         string
		allowPublic  bool
		authed       bool
		responseID   int64
		wantStatus   int
		wantLocation string
	}{
		{
			name:         "anonymous is redirected to login when public mementos off",
			allowPublic:  false,
			authed:       false,
			responseID:   responseID,
			wantStatus:   http.StatusSeeOther,
			wantLocation: "/login",
		},
		{
			name:        "member sees memento when public mementos off",
			allowPublic: false,
			authed:      true,
			responseID:  responseID,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "anonymous sees memento when public mementos on",
			allowPublic: true,
			authed:      false,
			responseID:  responseID,
			wantStatus:  http.StatusOK,
		},
		{
			name:        "unknown memento is 404",
			allowPublic: true,
			authed:      false,
			responseID:  999999,
			wantStatus:  http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env.setPublicMementos(t, tt.allowPublic)

			req := httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/m/%d", tt.responseID), nil)
			if tt.authed {
				req.AddCookie(session)
			}

			rr := env.do(t, req)

			require.Equal(t, tt.wantStatus, rr.Code, "body: %s", rr.Body.String())

			if tt.wantLocation != "" {
				assert.Equal(t, tt.wantLocation, rr.Header().Get("Location"))
			}

			if tt.wantStatus == http.StatusOK {
				assert.Contains(t, rr.Body.String(), "Integration question 1?")
			}
		})
	}
}

// TestIssuePageQuestionClamping verifies the ?q deep-link parameter is
// clamped server-side to [1, len(questions)] on the published issue page.
func TestIssuePageQuestionClamping(t *testing.T) {
	env := newIntegrationEnv(t)

	user := env.createUser(t, "Grace", "grace@example.com")
	session := env.sessionCookie(t, user.ID)
	env.seedIssue(t, "published", 5, 2026, 3)

	tests := []struct {
		name string
		q    string
		want string
	}{
		{
			name: "above range clamps to last question",
			q:    "99",
			want: "Question 3 of 3",
		},
		{
			name: "zero falls back to first question",
			q:    "0",
			want: "Question 1 of 3",
		},
		{
			name: "garbage falls back to first question",
			q:    "banana",
			want: "Question 1 of 3",
		},
		{
			name: "in-range value is honored",
			q:    "2",
			want: "Question 2 of 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/issues/2026/5?q="+tt.q, nil)
			req.AddCookie(session)

			rr := env.do(t, req)

			require.Equal(t, http.StatusOK, rr.Code)
			assert.Contains(t, rr.Body.String(), tt.want)
		})
	}
}

// TestManualPublishSchedulesNextIssue verifies that publishing an issue
// through the admin API queues the create_next_issue scheduler event when
// auto-create is on (previously only the scheduler's auto-publish path did,
// so manually published loops stalled on "no active issue"), and that the
// admin dashboard + member archive surface the scheduled open time.
func TestManualPublishSchedulesNextIssue(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	settings, err := env.store.GetSettings(ctx)
	require.NoError(t, err, "load settings")
	settings.AutoCreateEnabled = true
	require.NoError(t, env.store.UpdateSettings(ctx, settings), "enable auto-create")

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, _ := env.seedIssue(t, "collecting", 6, 2026, 2)

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issueID), nil)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)

	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code, "publish: %s", rr.Body.String())

	ev, err := env.store.GetNextPendingEventByType(ctx, "create_next_issue")
	require.NoError(t, err, "query next event")
	require.NotNil(t, ev, "manual publish must queue create_next_issue")
	assert.True(t, ev.ScheduledAt.After(time.Now()), "next issue opens in the future")

	// Admin dashboard shows when the next round opens.
	dashReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashReq.AddCookie(session)
	dash := env.do(t, dashReq)
	require.Equal(t, http.StatusOK, dash.Code)
	assert.Contains(t, dash.Body.String(), "opens automatically on")

	// Member-facing archive shows the same schedule.
	archReq := httptest.NewRequest(http.MethodGet, "/issues", nil)
	archReq.AddCookie(session)
	arch := env.do(t, archReq)
	require.Equal(t, http.StatusOK, arch.Code)
	assert.Contains(t, arch.Body.String(), "The next issue opens")
}

// TestManualPublishRespectsAutoCreateOff verifies no next-issue event is
// queued when the admin has auto-create disabled. Auto-create is seeded ON
// by default, so the test turns it off explicitly.
func TestManualPublishRespectsAutoCreateOff(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	settings, err := env.store.GetSettings(ctx)
	require.NoError(t, err, "load settings")
	settings.AutoCreateEnabled = false
	require.NoError(t, env.store.UpdateSettings(ctx, settings), "disable auto-create")

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, _ := env.seedIssue(t, "collecting", 6, 2026, 1)

	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issueID), nil)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)

	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code, "publish: %s", rr.Body.String())

	ev, err := env.store.GetNextPendingEventByType(ctx, "create_next_issue")
	require.NoError(t, err, "query next event")
	assert.Nil(t, ev, "auto-create off must not queue a next issue")
}

// TestDumpUploadLifecycle exercises the photo & video dump API end to end:
// a member uploads a photo to a collecting issue, it renders on the respond
// page and (after publish) on the issue's collage page, deletes are owner-
// scoped, and the dump closes once the issue is published.
func TestDumpUploadLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	member := env.createUser(t, "Zara", "zara@example.com")
	other := env.createUser(t, "Anil", "anil@example.com")
	session := env.sessionCookie(t, member.ID)
	otherSession := env.sessionCookie(t, other.ID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, _ := env.seedIssue(t, "collecting", 6, 2026, 1)

	// A tiny valid PNG (1×1) so content sniffing accepts the upload.
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	upload := func(sess *http.Cookie) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("media", "dump.png")
		require.NoError(t, err)
		_, err = part.Write(png)
		require.NoError(t, err)
		require.NoError(t, mw.WriteField("kind", "photo"))
		require.NoError(t, mw.Close())

		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/issues/%d/dump", issueID), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.AddCookie(sess)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)

		return env.do(t, req)
	}

	rr := upload(session)
	require.Equal(t, http.StatusCreated, rr.Code, "upload: %s", rr.Body.String())

	items, err := env.store.ListDumpItemsForUser(ctx, issueID, member.ID)
	require.NoError(t, err)
	require.Len(t, items, 1)

	// The respond page shows the member's own dump thumbnail.
	respondReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", issueID), nil)
	respondReq.AddCookie(session)
	respond := env.do(t, respondReq)
	require.Equal(t, http.StatusOK, respond.Code)
	assert.Contains(t, respond.Body.String(), "pl-dump-thumb")

	// Another member cannot delete someone else's item.
	delReq := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/dump/%d", items[0].ID), nil)
	delReq.AddCookie(otherSession)
	delReq.AddCookie(csrfCookie)
	delReq.Header.Set("X-CSRF-Token", csrfHeader)
	require.Equal(t, http.StatusForbidden, env.do(t, delReq).Code)

	// Publish: the collage closer renders on the issue page, and the dump
	// no longer accepts uploads or deletes.
	require.NoError(t, env.store.PublishIssue(ctx, issueID))

	pageReq := httptest.NewRequest(http.MethodGet, "/issues/2026/6", nil)
	pageReq.AddCookie(session)
	page := env.do(t, pageReq)
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "The photo &amp; video dump")
	assert.Contains(t, page.Body.String(), "pl-collage-item")
	assert.Contains(t, page.Body.String(), "Zara")

	// ?q=2 (question count 1 + dump page) resolves to the dump page rather
	// than clamping back to the last question.
	deepReq := httptest.NewRequest(http.MethodGet, "/issues/2026/6?q=2", nil)
	deepReq.AddCookie(session)
	deep := env.do(t, deepReq)
	require.Equal(t, http.StatusOK, deep.Code)
	assert.Contains(t, deep.Body.String(), "Question 2 of 2",
		"?q clamp must allow the dump page beyond the question count")

	require.Equal(t, http.StatusConflict, upload(session).Code,
		"dump must close at publish")

	ownDelReq := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/dump/%d", items[0].ID), nil)
	ownDelReq.AddCookie(session)
	ownDelReq.AddCookie(csrfCookie)
	ownDelReq.Header.Set("X-CSRF-Token", csrfHeader)
	require.Equal(t, http.StatusConflict, env.do(t, ownDelReq).Code,
		"deletes must close at publish")
}

// TestUpcomingDraftLifecycle covers the pre-created next round: publishing
// an issue creates a draft that accepts question suggestions (but not
// answers) until the scheduler opens it, at which point the suggestions
// lead the question list.
func TestUpcomingDraftLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, _ := env.seedIssue(t, "collecting", 6, 2026, 1)

	pubReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issueID), nil)
	pubReq.AddCookie(session)
	pubReq.AddCookie(csrfCookie)
	pubReq.Header.Set("X-CSRF-Token", csrfHeader)
	require.Equal(t, http.StatusOK, env.do(t, pubReq).Code)

	// Publish pre-created the next round as an upcoming draft.
	draft, err := env.store.GetUpcomingDraftIssue(ctx)
	require.NoError(t, err)
	require.NotNil(t, draft, "publish must pre-create the next draft")
	assert.Equal(t, "draft", draft.Status)
	assert.True(t, draft.OpensAt.After(time.Now()), "draft opens in the future")

	// The upcoming draft is not the active issue — the dashboard keeps its
	// "no active issue / next opens" framing.
	active, err := env.store.HasActiveIssue(ctx)
	require.NoError(t, err)
	assert.False(t, active, "future draft must not count as active")

	// The published reading view offers the next-issue suggestion card.
	readReq := httptest.NewRequest(http.MethodGet, "/issues/2026/6", nil)
	readReq.AddCookie(session)
	read := env.do(t, readReq)
	require.Equal(t, http.StatusOK, read.Code)
	assert.Contains(t, read.Body.String(), "Suggest a question for the next issue")

	// Members can suggest questions to the draft...
	suggest := func() *httptest.ResponseRecorder {
		body := strings.NewReader(fmt.Sprintf(
			`{"issue_id": %d, "text": "What did the last issue make you want to ask?"}`, draft.ID))
		req := httptest.NewRequest(http.MethodPost, "/api/questions/submit", body)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)
		return env.do(t, req)
	}
	require.Equal(t, http.StatusCreated, suggest().Code, "suggestions land in the upcoming draft")

	// ...but cannot answer it yet.
	respondReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", draft.ID), nil)
	respondReq.AddCookie(session)
	require.Equal(t, http.StatusForbidden, env.do(t, respondReq).Code,
		"draft must not accept answers before it opens")

	// The scheduler fires: the draft opens with the suggestion leading.
	require.NoError(t, env.srv.CreateNextIssue(ctx))

	opened, err := env.store.GetIssueByID(ctx, draft.ID)
	require.NoError(t, err)
	assert.Equal(t, "collecting", opened.Status)

	questions, err := env.store.ListQuestionsByIssue(ctx, draft.ID)
	require.NoError(t, err)
	require.NotEmpty(t, questions)
	assert.Equal(t, "friend", questions[0].Source, "suggestion stays first")

	// Now the round is open: answering works, further "next round"
	// suggestions to a published issue are refused.
	respondReq2 := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", draft.ID), nil)
	respondReq2.AddCookie(session)
	require.Equal(t, http.StatusOK, env.do(t, respondReq2).Code)
}

// TestLiveQuestionEditing covers the dashboard's question editor API flow:
// admins can add (source defaults to 'admin' — previously rejected by the
// schema CHECK), edit, and remove questions on a collecting issue, and the
// dashboard exposes per-question answer counts for the "already answered"
// warning.
func TestLiveQuestionEditing(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	member := env.createUser(t, "Zara", "zara@example.com")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, questionIDs := env.seedIssue(t, "collecting", 6, 2026, 2)

	// Member answers question 1 — that question must carry a warning count.
	respID, err := env.store.CreateResponse(ctx, member.ID, questionIDs[0])
	require.NoError(t, err)
	_ = respID

	counts, err := env.store.CountResponsesByQuestion(ctx, issueID)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[questionIDs[0]])
	assert.Equal(t, 0, counts[questionIDs[1]])

	authed := func(req *http.Request) *http.Request {
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)
		return req
	}

	// Add with default source (the old CHECK constraint rejected 'admin').
	addReq := authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/questions", issueID),
		`{"text": "A brand new prompt?"}`))
	addRR := env.do(t, addReq)
	require.Equal(t, http.StatusCreated, addRR.Code, "add: %s", addRR.Body.String())
	assert.Contains(t, addRR.Body.String(), `"source":"admin"`)

	// Edit the answered question — allowed (the warning is client-side).
	editReq := authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/questions/%d", questionIDs[0]),
		`{"text": "Integration question 1, reworded?"}`))
	editRR := env.do(t, editReq)
	require.Equal(t, http.StatusOK, editRR.Code, "edit: %s", editRR.Body.String())

	q, err := env.store.GetQuestionByID(ctx, questionIDs[0])
	require.NoError(t, err)
	assert.Equal(t, "Integration question 1, reworded?", q.Text)

	// Remove the unanswered question.
	delReq := authed(httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/questions/%d", questionIDs[1]), nil))
	require.Equal(t, http.StatusNoContent, env.do(t, delReq).Code)

	// Dashboard renders the editor with the answered warning count.
	dashReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashReq.AddCookie(session)
	dash := env.do(t, dashReq)
	require.Equal(t, http.StatusOK, dash.Code)
	assert.Contains(t, dash.Body.String(), "Questions in this round")
	assert.Contains(t, dash.Body.String(), "1 member already answered")
	assert.Contains(t, dash.Body.String(), "Integration question 1, reworded?")
}
