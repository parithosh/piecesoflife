package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/parithosh/piecesoflife/internal/config"
)

// JMAP capability URNs used in every API request.
const (
	jmapCoreCap       = "urn:ietf:params:jmap:core"
	jmapMailCap       = "urn:ietf:params:jmap:mail"
	jmapSubmissionCap = "urn:ietf:params:jmap:submission"
)

// jmapTransport delivers mail through a JMAP provider (e.g. Fastmail) using
// a bearer API token. On first use it resolves the session (account id, API
// endpoint), the sending identity matching the From address, and the Drafts
// and Sent mailbox ids, then caches them for the lifetime of the process.
type jmapTransport struct {
	sessionURL string
	token      string
	from       string
	fromName   string
	client     *http.Client
	logger     *slog.Logger

	mu sync.Mutex
	// cached after the first successful init
	apiURL     string
	accountID  string
	identityID string
	draftsID   string
	sentID     string
	ready      bool
}

// jmapSession is the subset of the JMAP session resource we need.
type jmapSession struct {
	APIURL          string                        `json:"apiUrl"`
	PrimaryAccounts map[string]string             `json:"primaryAccounts"`
	Accounts        map[string]struct{ Name any } `json:"accounts"`
}

// jmapResponse is the generic shape of a JMAP API response.
type jmapResponse struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
}

// jmapHTTPError is a non-200 response from the session or API endpoint. The
// status code is kept so send() can distinguish stale-session failures
// (unauthorized, moved) from per-message rejections.
type jmapHTTPError struct {
	status int
	body   string
}

func (e *jmapHTTPError) Error() string {
	return fmt.Sprintf("JMAP endpoint returned %d: %s", e.status, e.body)
}

// sessionStale reports whether an error looks like the cached session state
// (token, API URL, account or mailbox ids) is no longer valid — the cases a
// re-bootstrap can actually fix. Per-message SetErrors (bad recipient,
// forbidden From) deliberately don't match.
func sessionStale(err error) bool {
	var httpErr *jmapHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.status {
		case http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusGone:
			return true
		}

		return false
	}

	// Method-level "error" responses carry a JMAP error type; the account /
	// state ones are recoverable by re-resolving the session.
	msg := err.Error()

	return strings.Contains(msg, "accountNotFound") ||
		strings.Contains(msg, "accountNotSupportedByMethod")
}

func newJMAPTransport(cfg *config.Config, logger *slog.Logger) *jmapTransport {
	return &jmapTransport{
		sessionURL: cfg.JMAPSessionURL,
		token:      cfg.JMAPAPIToken,
		from:       cfg.FromEmail,
		fromName:   cfg.FromName,
		client:     &http.Client{Timeout: 30 * time.Second},
		logger:     logger,
	}
}

// send delivers one email. If the attempt fails in a way that suggests the
// cached session state is stale (token rotated, mailbox ids changed, API URL
// moved), the cache is dropped and the send retried once against a freshly
// bootstrapped session.
func (t *jmapTransport) send(ctx context.Context, to, subject, htmlBody string) error {
	err := t.sendOnce(ctx, to, subject, htmlBody)
	if err == nil || !sessionStale(err) {
		return err
	}

	t.logger.WarnContext(ctx, "JMAP send failed with stale-session signature — re-bootstrapping and retrying",
		slog.String("error", err.Error()),
	)
	t.invalidate()

	return t.sendOnce(ctx, to, subject, htmlBody)
}

func (t *jmapTransport) sendOnce(ctx context.Context, to, subject, htmlBody string) error {
	if err := t.ensureSession(ctx); err != nil {
		return fmt.Errorf("initializing JMAP session: %w", err)
	}

	// One API call: create the email in Drafts, submit it, and on success
	// move it to Sent and clear the $draft keyword.
	request := map[string]any{
		"using": []string{jmapCoreCap, jmapMailCap, jmapSubmissionCap},
		"methodCalls": []any{
			[]any{"Email/set", map[string]any{
				"accountId": t.accountID,
				"create": map[string]any{
					"draft": map[string]any{
						"mailboxIds":    map[string]bool{t.draftsID: true},
						"keywords":      map[string]bool{"$draft": true},
						"from":          []map[string]string{t.fromAddress()},
						"to":            []map[string]string{{"email": to}},
						"subject":       subject,
						"bodyStructure": map[string]any{"type": "text/html", "partId": "body"},
						"bodyValues": map[string]any{
							"body": map[string]any{"value": htmlBody},
						},
					},
				},
			}, "0"},
			[]any{"EmailSubmission/set", map[string]any{
				"accountId": t.accountID,
				"create": map[string]any{
					"sub": map[string]any{
						"emailId":    "#draft",
						"identityId": t.identityID,
					},
				},
				"onSuccessUpdateEmail": map[string]any{
					"#sub": map[string]any{
						"mailboxIds/" + t.draftsID: nil,
						"mailboxIds/" + t.sentID:   true,
						"keywords/$draft":          nil,
					},
				},
			}, "1"},
		},
	}

	resp, err := t.api(ctx, request)
	if err != nil {
		return fmt.Errorf("sending email to %s via JMAP: %w", to, err)
	}

	if err := jmapSetError(resp, 0, "Email/set", "draft", "notCreated"); err != nil {
		return fmt.Errorf("creating draft for %s: %w", to, err)
	}

	if err := jmapSetError(resp, 1, "EmailSubmission/set", "sub", "notCreated"); err != nil {
		return fmt.Errorf("submitting email to %s: %w", to, err)
	}

	return nil
}

// fromAddress builds the JMAP EmailAddress object for the From header,
// including the display name when one is configured.
func (t *jmapTransport) fromAddress() map[string]string {
	addr := map[string]string{"email": t.from}
	if t.fromName != "" {
		addr["name"] = t.fromName
	}

	return addr
}

// invalidate drops the cached session so the next send re-bootstraps.
func (t *jmapTransport) invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ready = false
}

// ensureSession lazily resolves and caches the session, identity, and
// mailbox ids needed for sending.
func (t *jmapTransport) ensureSession(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.ready {
		return nil
	}

	// 1. Session resource → API URL + account id for submission.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.sessionURL, nil)
	if err != nil {
		return fmt.Errorf("building session request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)

	res, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching session: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("fetching session: %w",
			&jmapHTTPError{status: res.StatusCode, body: strings.TrimSpace(string(body))})
	}

	var session jmapSession
	if err := json.NewDecoder(res.Body).Decode(&session); err != nil {
		return fmt.Errorf("decoding session: %w", err)
	}

	accountID := session.PrimaryAccounts[jmapSubmissionCap]
	if accountID == "" {
		accountID = session.PrimaryAccounts[jmapMailCap]
	}
	if accountID == "" || session.APIURL == "" {
		return fmt.Errorf("session missing apiUrl or a %s primary account", jmapSubmissionCap)
	}

	t.apiURL = session.APIURL
	t.accountID = accountID

	// 2. Identity matching the From address + Drafts/Sent mailbox ids.
	bootstrap := map[string]any{
		"using": []string{jmapCoreCap, jmapMailCap, jmapSubmissionCap},
		"methodCalls": []any{
			[]any{"Identity/get", map[string]any{"accountId": accountID}, "0"},
			[]any{"Mailbox/get", map[string]any{
				"accountId":  accountID,
				"properties": []string{"id", "role"},
			}, "1"},
		},
	}

	resp, err := t.api(ctx, bootstrap)
	if err != nil {
		return fmt.Errorf("bootstrapping identity and mailboxes: %w", err)
	}

	identityID, err := t.pickIdentity(resp)
	if err != nil {
		return err
	}

	draftsID, sentID, err := pickMailboxes(resp)
	if err != nil {
		return err
	}

	t.identityID = identityID
	t.draftsID = draftsID
	t.sentID = sentID
	t.ready = true

	t.logger.Info("JMAP session initialized",
		slog.String("api_url", t.apiURL),
		slog.String("account_id", t.accountID),
		slog.String("identity_id", t.identityID),
	)

	return nil
}

// api posts a JMAP request and decodes the generic response envelope.
func (t *jmapTransport) api(ctx context.Context, payload map[string]any) (*jmapResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling JMAP API: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, &jmapHTTPError{
			status: res.StatusCode,
			body:   strings.TrimSpace(string(respBody)),
		}
	}

	var decoded jmapResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decoding API response: %w", err)
	}

	return &decoded, nil
}

// pickIdentity chooses the identity whose email matches the From address,
// falling back to the first identity when none matches exactly.
func (t *jmapTransport) pickIdentity(resp *jmapResponse) (string, error) {
	payload, err := methodPayload(resp, 0, "Identity/get")
	if err != nil {
		return "", err
	}

	var identities struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"list"`
	}
	if err := json.Unmarshal(payload, &identities); err != nil {
		return "", fmt.Errorf("decoding identities: %w", err)
	}

	if len(identities.List) == 0 {
		return "", fmt.Errorf("no JMAP sending identities available for this token")
	}

	for _, identity := range identities.List {
		if strings.EqualFold(identity.Email, t.from) {
			return identity.ID, nil
		}
	}

	t.logger.Warn("No JMAP identity matches FROM_EMAIL — using the first identity",
		slog.String("from", t.from),
		slog.String("identity_email", identities.List[0].Email),
	)

	return identities.List[0].ID, nil
}

// pickMailboxes extracts the Drafts and Sent mailbox ids from a Mailbox/get
// response.
func pickMailboxes(resp *jmapResponse) (draftsID, sentID string, err error) {
	payload, err := methodPayload(resp, 1, "Mailbox/get")
	if err != nil {
		return "", "", err
	}

	var mailboxes struct {
		List []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"list"`
	}
	if err := json.Unmarshal(payload, &mailboxes); err != nil {
		return "", "", fmt.Errorf("decoding mailboxes: %w", err)
	}

	for _, mb := range mailboxes.List {
		switch mb.Role {
		case "drafts":
			draftsID = mb.ID
		case "sent":
			sentID = mb.ID
		}
	}

	if draftsID == "" || sentID == "" {
		return "", "", fmt.Errorf("account is missing a drafts or sent mailbox (drafts=%q sent=%q)", draftsID, sentID)
	}

	return draftsID, sentID, nil
}

// methodPayload returns the arguments object of methodResponses[idx] after
// verifying the method name matches (JMAP errors come back as a method named
// "error").
func methodPayload(resp *jmapResponse, idx int, want string) (json.RawMessage, error) {
	if idx >= len(resp.MethodResponses) {
		return nil, fmt.Errorf("JMAP response missing %s (only %d method responses)", want, len(resp.MethodResponses))
	}

	var envelope []json.RawMessage
	if err := json.Unmarshal(resp.MethodResponses[idx], &envelope); err != nil || len(envelope) < 2 {
		return nil, fmt.Errorf("malformed JMAP method response for %s", want)
	}

	var name string
	if err := json.Unmarshal(envelope[0], &name); err != nil {
		return nil, fmt.Errorf("malformed JMAP method name for %s", want)
	}

	if name == "error" {
		return nil, fmt.Errorf("JMAP %s failed: %s", want, strings.TrimSpace(string(envelope[1])))
	}

	if name != want {
		return nil, fmt.Errorf("unexpected JMAP method response %q (want %s)", name, want)
	}

	return envelope[1], nil
}

// jmapSetError checks a */set method response for a not-created entry under
// the given creation id and surfaces its SetError description.
func jmapSetError(resp *jmapResponse, idx int, method, creationID, notKey string) error {
	payload, err := methodPayload(resp, idx, method)
	if err != nil {
		return err
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(payload, &result); err != nil {
		return fmt.Errorf("decoding %s result: %w", method, err)
	}

	rawNot, ok := result[notKey]
	if !ok || string(rawNot) == "null" {
		return nil
	}

	var notCreated map[string]json.RawMessage
	if err := json.Unmarshal(rawNot, &notCreated); err != nil {
		return fmt.Errorf("decoding %s.%s: %w", method, notKey, err)
	}

	if detail, failed := notCreated[creationID]; failed {
		return fmt.Errorf("%s rejected %q: %s", method, creationID, strings.TrimSpace(string(detail)))
	}

	return nil
}
