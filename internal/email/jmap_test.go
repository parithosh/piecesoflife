package email

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/config"
)

// fakeJMAP is a minimal Fastmail-shaped JMAP server: it serves a session
// document and answers Identity/get + Mailbox/get bootstrap calls and
// Email/set + EmailSubmission/set send calls.
type fakeJMAP struct {
	t *testing.T

	srv *httptest.Server

	sessionHits int
	apiHits     int
	lastAuth    string
	lastSend    map[string]any

	failSubmission bool
	// reject401 makes the next N API calls fail with 401, simulating a
	// server-side session/token invalidation that a re-bootstrap resolves.
	reject401 int
}

func newFakeJMAP(t *testing.T) *fakeJMAP {
	f := &fakeJMAP{t: t}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /jmap/session", f.handleSession)
	mux.HandleFunc("POST /jmap/api", f.handleAPI)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeJMAP) handleSession(w http.ResponseWriter, r *http.Request) {
	f.sessionHits++
	f.lastAuth = r.Header.Get("Authorization")

	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiUrl": f.srv.URL + "/jmap/api",
		"primaryAccounts": map[string]string{
			jmapSubmissionCap: "acc-1",
			jmapMailCap:       "acc-1",
		},
	})
}

func (f *fakeJMAP) handleAPI(w http.ResponseWriter, r *http.Request) {
	f.apiHits++
	f.lastAuth = r.Header.Get("Authorization")

	if f.reject401 > 0 {
		f.reject401--
		http.Error(w, "session expired", http.StatusUnauthorized)

		return
	}

	var req struct {
		MethodCalls [][3]json.RawMessage `json:"methodCalls"`
	}
	require.NoError(f.t, json.NewDecoder(r.Body).Decode(&req))

	responses := make([]any, 0, len(req.MethodCalls))

	for _, call := range req.MethodCalls {
		var name string
		require.NoError(f.t, json.Unmarshal(call[0], &name))

		switch name {
		case "Identity/get":
			responses = append(responses, []any{"Identity/get", map[string]any{
				"list": []map[string]string{
					{"id": "ident-other", "email": "other@example.com"},
					{"id": "ident-1", "email": "loop@example.com"},
				},
			}, "0"})

		case "Mailbox/get":
			responses = append(responses, []any{"Mailbox/get", map[string]any{
				"list": []map[string]string{
					{"id": "mb-inbox", "role": "inbox"},
					{"id": "mb-drafts", "role": "drafts"},
					{"id": "mb-sent", "role": "sent"},
				},
			}, "1"})

		case "Email/set":
			var args map[string]any
			require.NoError(f.t, json.Unmarshal(call[1], &args))
			f.lastSend = args
			responses = append(responses, []any{"Email/set", map[string]any{
				"created": map[string]any{"draft": map[string]string{"id": "email-1"}},
			}, "0"})

		case "EmailSubmission/set":
			if f.failSubmission {
				responses = append(responses, []any{"EmailSubmission/set", map[string]any{
					"notCreated": map[string]any{
						"sub": map[string]string{
							"type":        "forbiddenFrom",
							"description": "sender not allowed",
						},
					},
				}, "1"})
				break
			}
			responses = append(responses, []any{"EmailSubmission/set", map[string]any{
				"created": map[string]any{"sub": map[string]string{"id": "sub-1"}},
			}, "1"})

		default:
			responses = append(responses, []any{"error", map[string]string{"type": "unknownMethod"}, "x"})
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses})
}

func (f *fakeJMAP) transport() *jmapTransport {
	cfg := &config.Config{
		EmailProvider:  "jmap",
		FromEmail:      "loop@example.com",
		JMAPSessionURL: f.srv.URL + "/jmap/session",
		JMAPAPIToken:   "test-token",
	}

	return newJMAPTransport(cfg, slog.Default())
}

func TestJMAPSendHappyPath(t *testing.T) {
	fake := newFakeJMAP(t)
	tr := fake.transport()

	err := tr.send(context.Background(), "friend@example.com", "Hello", `<p>Hi <a href="https://x">x</a></p>`)
	require.NoError(t, err)

	assert.Equal(t, "Bearer test-token", fake.lastAuth)
	assert.Equal(t, 1, fake.sessionHits, "session fetched once")
	assert.Equal(t, 2, fake.apiHits, "bootstrap + send")

	// The matching identity (not the first one) must be cached.
	assert.Equal(t, "ident-1", tr.identityID)
	assert.Equal(t, "mb-drafts", tr.draftsID)
	assert.Equal(t, "mb-sent", tr.sentID)

	// The created draft carries the recipient, subject, and HTML body.
	create := fake.lastSend["create"].(map[string]any)["draft"].(map[string]any)
	to := create["to"].([]any)[0].(map[string]any)
	assert.Equal(t, "friend@example.com", to["email"])
	assert.Equal(t, "Hello", create["subject"])
	body := create["bodyValues"].(map[string]any)["body"].(map[string]any)
	assert.Contains(t, body["value"], "Hi")

	// Second send reuses the cached session (no extra session/bootstrap hit).
	require.NoError(t, tr.send(context.Background(), "b@example.com", "Again", "<p>2</p>"))
	assert.Equal(t, 1, fake.sessionHits)
	assert.Equal(t, 3, fake.apiHits)
}

func TestJMAPStaleSessionRetriesOnce(t *testing.T) {
	fake := newFakeJMAP(t)
	tr := fake.transport()

	// Prime the cached session with a successful send.
	require.NoError(t, tr.send(context.Background(), "a@example.com", "s1", "<p>1</p>"))
	require.Equal(t, 1, fake.sessionHits)

	// Server invalidates the session: next API call 401s. The transport
	// must drop its cache, re-bootstrap, and deliver on the retry.
	fake.reject401 = 1
	require.NoError(t, tr.send(context.Background(), "b@example.com", "s2", "<p>2</p>"))

	assert.Equal(t, 2, fake.sessionHits, "session re-fetched after 401")

	to := fake.lastSend["create"].(map[string]any)["draft"].(map[string]any)["to"].([]any)[0].(map[string]any)
	assert.Equal(t, "b@example.com", to["email"], "retried send delivered")
}

func TestJMAPSubmissionRejectionDoesNotRetry(t *testing.T) {
	fake := newFakeJMAP(t)
	fake.failSubmission = true
	tr := fake.transport()

	err := tr.send(context.Background(), "friend@example.com", "Hello", "<p>Hi</p>")
	require.Error(t, err)

	// One bootstrap + one send — a per-message rejection must not trigger
	// the stale-session retry (it would just double the rejection).
	assert.Equal(t, 2, fake.apiHits)
	assert.Equal(t, 1, fake.sessionHits)
}

func TestJMAPFromNameIncluded(t *testing.T) {
	fake := newFakeJMAP(t)
	tr := fake.transport()
	tr.fromName = "Chaos Crew"

	require.NoError(t, tr.send(context.Background(), "a@example.com", "s", "<p>b</p>"))

	from := fake.lastSend["create"].(map[string]any)["draft"].(map[string]any)["from"].([]any)[0].(map[string]any)
	assert.Equal(t, "loop@example.com", from["email"])
	assert.Equal(t, "Chaos Crew", from["name"])
}

func TestJMAPSendSubmissionRejected(t *testing.T) {
	fake := newFakeJMAP(t)
	fake.failSubmission = true
	tr := fake.transport()

	err := tr.send(context.Background(), "friend@example.com", "Hello", "<p>Hi</p>")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbiddenFrom")
}

func TestJMAPSessionAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	tr := newJMAPTransport(&config.Config{
		FromEmail:      "loop@example.com",
		JMAPSessionURL: srv.URL,
		JMAPAPIToken:   "wrong",
	}, slog.Default())

	err := tr.send(context.Background(), "a@example.com", "s", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestSenderProviderSelection(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		wantType any
	}{
		{name: "jmap selects jmapTransport", provider: "jmap", wantType: &jmapTransport{}},
		{name: "smtp selects smtpTransport", provider: "smtp", wantType: &smtpTransport{}},
		{name: "default selects smtpTransport", provider: "", wantType: &smtpTransport{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSender(&config.Config{
				EmailProvider:  tc.provider,
				JMAPSessionURL: "https://example.com/session",
			}, slog.Default())
			assert.IsType(t, tc.wantType, s.transport)
		})
	}
}
