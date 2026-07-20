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
	"os"
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
// exempt from CSRF and must never reveal whether an email address exists —
// including through the rate limiter: known and unknown addresses must
// produce the identical response sequence (3× sent, then rate_limited).
func TestLoginFormNoJS(t *testing.T) {
	env := newIntegrationEnv(t)

	env.createUser(t, "Dave", "dave@example.com")

	post := func(email string) string {
		form := url.Values{"email": {email}}
		req := httptest.NewRequest(http.MethodPost, "/login",
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		rr := env.do(t, req)
		require.Equal(t, http.StatusSeeOther, rr.Code)

		return rr.Header().Get("Location")
	}

	assert.Equal(t, "/login?error=email", post("invalid"), "malformed email")

	// Identical distribution for a real account and a ghost: three sends,
	// then the rate limiter — indistinguishable from the outside.
	for _, email := range []string{"dave@example.com", "ghost@example.com"} {
		for i := 1; i <= 3; i++ {
			assert.Equal(t, "/login?sent=1", post(email),
				"%s attempt %d should read as sent", email, i)
		}
		assert.Equal(t, "/login?error=rate_limited", post(email),
			"%s attempt 4 should rate limit", email)
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

	settings, err := env.store.GetSettings(ctx, 1)
	require.NoError(t, err, "load settings")
	settings.AutoCreateEnabled = true
	require.NoError(t, env.store.UpdateSettings(ctx, settings), "enable auto-create")

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
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

	settings, err := env.store.GetSettings(ctx, 1)
	require.NoError(t, err, "load settings")
	settings.AutoCreateEnabled = false
	require.NoError(t, env.store.UpdateSettings(ctx, settings), "disable auto-create")

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
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

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
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
	draft, err := env.store.GetUpcomingDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft, "publish must pre-create the next draft")
	assert.Equal(t, "draft", draft.Status)
	assert.True(t, draft.OpensAt.After(time.Now()), "draft opens in the future")

	// The upcoming draft is not the active issue — the dashboard keeps its
	// "no active issue / next opens" framing.
	active, err := env.store.HasActiveIssue(ctx, 1)
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
	require.NoError(t, env.srv.CreateNextIssue(ctx, 1, draft.OpensAt))

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

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
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

	// Reorder: reverse the remaining questions and save the new order.
	remaining, err := env.store.ListQuestionsByIssue(ctx, issueID)
	require.NoError(t, err)

	reversed := make([]string, 0, len(remaining))
	for i := len(remaining) - 1; i >= 0; i-- {
		reversed = append(reversed, fmt.Sprintf("%d", remaining[i].ID))
	}

	reorderReq := authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/questions/reorder", issueID),
		fmt.Sprintf(`{"question_ids": [%s]}`, strings.Join(reversed, ","))))
	reorderRR := env.do(t, reorderReq)
	require.Equal(t, http.StatusOK, reorderRR.Code, "reorder: %s", reorderRR.Body.String())

	reordered, err := env.store.ListQuestionsByIssue(ctx, issueID)
	require.NoError(t, err)
	require.Len(t, reordered, len(remaining))
	assert.Equal(t, remaining[len(remaining)-1].ID, reordered[0].ID,
		"reversed order persisted")

	// A stale list (one ID missing) is rejected whole.
	staleReq := authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/questions/reorder", issueID),
		fmt.Sprintf(`{"question_ids": [%d]}`, remaining[0].ID)))
	assert.Equal(t, http.StatusConflict, env.do(t, staleReq).Code,
		"partial order must be rejected")

	// Duplicate IDs pass the count guard but not the uniqueness one.
	dupIDs := append([]string{}, reversed[:len(reversed)-1]...)
	dupIDs = append(dupIDs, reversed[0])
	dupReq := authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/questions/reorder", issueID),
		fmt.Sprintf(`{"question_ids": [%s]}`, strings.Join(dupIDs, ","))))
	assert.Equal(t, http.StatusConflict, env.do(t, dupReq).Code,
		"duplicate IDs must be rejected")

	// Members cannot suggest questions to the CURRENT round — suggestions
	// only land on the next (upcoming draft) issue.
	memberSession := env.sessionCookie(t, member.ID)
	suggestReq := newJSONRequest(http.MethodPost, "/api/questions/submit",
		fmt.Sprintf(`{"issue_id": %d, "text": "Sneaking one in?"}`, issueID))
	suggestReq.AddCookie(memberSession)
	suggestReq.AddCookie(csrfCookie)
	suggestReq.Header.Set("X-CSRF-Token", csrfHeader)
	assert.Equal(t, http.StatusConflict, env.do(t, suggestReq).Code,
		"collecting rounds accept no member suggestions")

	// Dashboard renders the editor with the answered warning count.
	dashReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	dashReq.AddCookie(session)
	dash := env.do(t, dashReq)
	require.Equal(t, http.StatusOK, dash.Code)
	assert.Contains(t, dash.Body.String(), "Questions in this round")
	assert.Contains(t, dash.Body.String(), "1 member already answered")
	assert.Contains(t, dash.Body.String(), "Integration question 1, reworded?")
}

// TestReminderScheduleAnchorsToDeadline locks in the reminder cadence: a
// reminder 3 days before the deadline, a final one 1 day before, both at a
// mid-morning local hour, and auto_close at the deadline. Short windows
// drop reminder slots that would fire with less than half a day's notice.
func TestReminderScheduleAnchorsToDeadline(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	authed := func(req *http.Request) *http.Request {
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)
		return req
	}

	// pendingFor collects the unfired events of one issue, keyed by type.
	pendingFor := func(issueID int64) map[string]time.Time {
		events, err := env.store.GetPendingEvents(ctx)
		require.NoError(t, err)

		byType := make(map[string]time.Time, 4)
		for _, ev := range events {
			if ev.IssueID != nil && *ev.IssueID == issueID {
				byType[ev.EventType] = ev.ScheduledAt
			}
		}

		return byType
	}

	// Default 7-day window: both reminders fit.
	createRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Round one"}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	events := pendingFor(issue.ID)
	require.Contains(t, events, "reminder_1", "7-day window keeps the 3-days-out reminder")
	require.Contains(t, events, "reminder_2", "7-day window keeps the last-chance reminder")
	require.Contains(t, events, "admin_summary", "admin gets the pre-publish roster")
	require.Contains(t, events, "auto_close")

	// The seeded loop timezone is UTC.
	deadline := issue.Deadline.In(time.UTC)
	r1 := events["reminder_1"].In(time.UTC)
	r2 := events["reminder_2"].In(time.UTC)

	assert.Equal(t, deadline.AddDate(0, 0, -3).Format("2006-01-02"), r1.Format("2006-01-02"),
		"reminder_1 lands 3 days before the deadline")
	assert.Equal(t, deadline.AddDate(0, 0, -1).Format("2006-01-02"), r2.Format("2006-01-02"),
		"reminder_2 lands 1 day before the deadline")
	assert.Equal(t, 10, r1.Hour(), "reminders fire mid-morning local time")
	assert.Equal(t, 10, r2.Hour(), "reminders fire mid-morning local time")
	assert.True(t, events["admin_summary"].Equal(events["reminder_2"]),
		"admin summary rides the last-chance slot, the day before publish")
	assert.True(t, events["auto_close"].Equal(issue.Deadline), "auto_close at the deadline")

	// A 3-day window puts the 3-days-out slot at (or before) the open —
	// it is dropped; the last-chance reminder survives.
	setRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		"/api/admin/settings", `{"submission_window_days": 3}`)))
	require.Equal(t, http.StatusOK, setRR.Code, "settings: %s", setRR.Body.String())

	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", issue.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	shortRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Short round"}`)))
	require.Equal(t, http.StatusCreated, shortRR.Code, "create short: %s", shortRR.Body.String())

	short, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	events = pendingFor(short.ID)
	assert.NotContains(t, events, "reminder_1", "3-day window drops the 3-days-out reminder")
	require.Contains(t, events, "reminder_2", "last-chance reminder survives a short window")
	require.Contains(t, events, "auto_close")

	// Extending to a deadline too close for any standard reminder slot
	// still nudges non-responders: an immediate reminder is queued in
	// place of the skipped slots (the extend dialog promises one).
	nearDeadline := time.Now().Add(20 * time.Hour)
	extRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/extend", short.ID),
		fmt.Sprintf(`{"new_deadline": %q}`, time.Now().Add(4*24*time.Hour).Format(time.RFC3339)))))
	require.Equal(t, http.StatusOK, extRR.Code, "extend far: %s", extRR.Body.String())

	// Shrink is not allowed via the API, so simulate a near deadline in
	// the store and extend by a few hours — no slot has 12h of lead.
	require.NoError(t, env.store.UpdateIssue(ctx, short.ID, nil, &nearDeadline))
	extRR = env.do(t, authed(newJSONRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/extend", short.ID),
		fmt.Sprintf(`{"new_deadline": %q}`, time.Now().Add(22*time.Hour).Format(time.RFC3339)))))
	require.Equal(t, http.StatusOK, extRR.Code, "extend near: %s", extRR.Body.String())

	events = pendingFor(short.ID)
	require.Contains(t, events, "reminder_2", "an immediate nudge replaces the skipped slots")
	assert.True(t, events["reminder_2"].Before(time.Now().Add(time.Minute)),
		"the fallback reminder fires on the next scheduler tick")
}

// TestEmailCTATokenReuse locks in the forgiving ?auth= semantics: email_cta
// tokens are reusable until expiry (mail scanners prefetch links, and a
// member may click an email button more than once), and a visitor with a
// live session is never bounced to a login error by a stale token.
func TestEmailCTATokenReuse(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Fay", "fay@example.com")

	raw, hash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAuthToken(
		ctx, user.ID, hash, "email_cta", time.Now().Add(30*24*time.Hour)))

	// First use (e.g. a mail scanner prefetch): mints a session and
	// redirects to the target without the auth param.
	rr := env.do(t, httptest.NewRequest(http.MethodGet, "/issues?auth="+raw, nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	assert.Equal(t, "/issues", rr.Header().Get("Location"))
	require.NotNil(t, findCookie(rr, "session"), "first use logs in")

	// Second cold use (the member's real click after the scanner's): the
	// token is not burned — it still logs in.
	rr2 := env.do(t, httptest.NewRequest(http.MethodGet, "/issues?auth="+raw, nil))
	require.Equal(t, http.StatusSeeOther, rr2.Code)
	assert.Equal(t, "/issues", rr2.Header().Get("Location"))
	assert.NotNil(t, findCookie(rr2, "session"), "email_cta tokens are reusable")

	// A visitor who is already logged in keeps their session — even when
	// the link carries an expired token.
	session := env.sessionCookie(t, user.ID)

	expRaw, expHash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAuthToken(
		ctx, user.ID, expHash, "email_cta", time.Now().Add(-time.Hour)))

	req := httptest.NewRequest(http.MethodGet, "/issues?auth="+expRaw, nil)
	req.AddCookie(session)
	rr3 := env.do(t, req)
	require.Equal(t, http.StatusSeeOther, rr3.Code)
	assert.Equal(t, "/issues", rr3.Header().Get("Location"),
		"a live session wins over a stale token")
}

// TestAdminSummaryEmail covers the pre-publish roster email: only admins
// receive it, sends land in the email log, and rounds that are no longer
// collecting are skipped silently.
func TestAdminSummaryEmail(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID
	member := env.createUser(t, "Zara", "zara@example.com")

	issueID, questionIDs := env.seedIssue(t, "collecting", 6, 2026, 2)

	respID, err := env.store.CreateResponse(ctx, member.ID, questionIDs[0])
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, respID))

	require.NoError(t, env.srv.SendAdminSummaryForIssue(ctx, issueID, nil))

	countSummaries := func() int {
		logs, _, err := env.store.ListEmailLogs(ctx, 1, 1, 100)
		require.NoError(t, err)

		n := 0
		for _, l := range logs {
			if l.Type != "admin_summary" {
				continue
			}
			n++
			require.NotNil(t, l.UserID)
			assert.Equal(t, adminID, *l.UserID, "only admins receive the roster")
			assert.Equal(t, "sent", l.Status)
		}

		return n
	}

	assert.Equal(t, 1, countSummaries(), "one summary, to the admin only")

	// Published rounds are skipped silently — no second log entry.
	require.NoError(t, env.store.PublishIssue(ctx, issueID))
	require.NoError(t, env.srv.SendAdminSummaryForIssue(ctx, issueID, nil))
	assert.Equal(t, 1, countSummaries(), "no summary after publish")
}

// TestAdminSettingsShowsEmailLog verifies the settings page renders the
// #email-log section the dashboard's "View log" link points at, with the
// recipient resolved from the users table.
func TestAdminSettingsShowsEmailLog(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	rita := env.createUser(t, "Rita Recipient", "rita@example.com")
	session := env.sessionCookie(t, admin.ID)

	groupID := int64(1)
	logID, err := env.store.LogEmail(ctx, &groupID, &rita.ID, nil,
		"invite", "pending", nil)
	require.NoError(t, err, "log email")

	now := time.Now()
	require.NoError(t, env.store.UpdateEmailLog(ctx, logID, "sent", &now, nil))

	req := httptest.NewRequest(http.MethodGet, "/admin/settings", nil)
	req.AddCookie(session)

	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	assert.Contains(t, body, `id="email-log"`, "email-log anchor exists")
	assert.Contains(t, body, "Rita Recipient", "recipient name is resolved")
	assert.Contains(t, body, "badge-green", "sent status renders as a green badge")
}

// TestAnswerPhotoDeleteLifecycle exercises removing an uploaded image from
// an answer: the upload lands on disk, deletes are owner-scoped, and a
// successful delete removes both the block row and the file.
func TestAnswerPhotoDeleteLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	member := env.createUser(t, "Zara", "zara@example.com")
	other := env.createUser(t, "Anil", "anil@example.com")
	session := env.sessionCookie(t, member.ID)
	otherSession := env.sessionCookie(t, other.ID)
	csrfCookie, csrfHeader := csrfPair()

	issueID, questionIDs := env.seedIssue(t, "collecting", 6, 2026, 1)

	responseID, err := env.store.CreateResponse(ctx, member.ID, questionIDs[0])
	require.NoError(t, err, "create response")

	// A tiny valid PNG (1×1) so content sniffing accepts the upload.
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("media", "answer.png")
	require.NoError(t, err)
	_, err = part.Write(png)
	require.NoError(t, err)
	require.NoError(t, mw.WriteField("kind", "photo"))
	require.NoError(t, mw.Close())

	upReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/responses/%d/blocks/upload", responseID), &buf)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upReq.AddCookie(session)
	upReq.AddCookie(csrfCookie)
	upReq.Header.Set("X-CSRF-Token", csrfHeader)

	up := env.do(t, upReq)
	require.Equal(t, http.StatusCreated, up.Code, "upload: %s", up.Body.String())

	blocks, err := env.store.ListBlocksByResponse(ctx, responseID)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].FilePath, "photo block stores its file path")

	filePath := *blocks[0].FilePath
	_, err = os.Stat(filePath)
	require.NoError(t, err, "uploaded file exists on disk")

	// The respond page offers the remove button for the saved photo.
	respondReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", issueID), nil)
	respondReq.AddCookie(session)
	respond := env.do(t, respondReq)
	require.Equal(t, http.StatusOK, respond.Code)
	assert.Contains(t, respond.Body.String(),
		fmt.Sprintf(`data-block-remove="%d"`, blocks[0].ID))

	deleteBlock := func(sess *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/blocks/%d", blocks[0].ID), nil)
		req.AddCookie(sess)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)

		return env.do(t, req)
	}

	// Another member cannot delete someone else's photo, and the file stays.
	require.Equal(t, http.StatusForbidden, deleteBlock(otherSession).Code)
	_, err = os.Stat(filePath)
	require.NoError(t, err, "file survives a forbidden delete")

	// The owner can: the block row and the file both go away.
	require.Equal(t, http.StatusNoContent, deleteBlock(session).Code)

	blocks, err = env.store.ListBlocksByResponse(ctx, responseID)
	require.NoError(t, err)
	assert.Empty(t, blocks, "block row deleted")

	_, err = os.Stat(filePath)
	assert.True(t, os.IsNotExist(err), "uploaded file removed from disk")
}

// TestAnswerPhotoUploadLimits verifies multiple photos can be added to one
// answer and each media type caps at 100 per response.
func TestAnswerPhotoUploadLimits(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	member := env.createUser(t, "Zara", "zara@example.com")
	session := env.sessionCookie(t, member.ID)
	csrfCookie, csrfHeader := csrfPair()

	_, questionIDs := env.seedIssue(t, "collecting", 6, 2026, 1)

	responseID, err := env.store.CreateResponse(ctx, member.ID, questionIDs[0])
	require.NoError(t, err, "create response")

	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
		0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	upload := func(kind string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("media", "answer.png")
		require.NoError(t, err)
		_, err = part.Write(png)
		require.NoError(t, err)
		require.NoError(t, mw.WriteField("kind", kind))
		require.NoError(t, mw.Close())

		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/api/responses/%d/blocks/upload", responseID), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)

		return env.do(t, req)
	}

	// Several photos on one answer are fine.
	for i := 0; i < 3; i++ {
		rr := upload("photo")
		require.Equal(t, http.StatusCreated, rr.Code, "photo %d: %s", i+1, rr.Body.String())
	}

	// Pad to the 100-photo cap directly in the store (the limit check runs
	// before the file is read, so blocks without files are fine here).
	for i := 3; i < 100; i++ {
		_, err := env.store.CreateBlock(ctx, responseID, "photo", nil, nil, nil, nil, i)
		require.NoError(t, err)
	}

	over := upload("photo")
	require.Equal(t, http.StatusUnprocessableEntity, over.Code)
	assert.Contains(t, over.Body.String(), "Maximum of 100 photo uploads")

	// Recordings cap at 100 per type as well: 100 audio blocks fill it.
	for i := 0; i < 100; i++ {
		_, err := env.store.CreateBlock(ctx, responseID, "audio", nil, nil, nil, nil, 100+i)
		require.NoError(t, err)
	}

	overAudio := upload("audio")
	require.Equal(t, http.StatusUnprocessableEntity, overAudio.Code)
	assert.Contains(t, overAudio.Body.String(), "Maximum of 100 audio uploads")
}
