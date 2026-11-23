package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/auth"
	"github.com/parithosh/piecesoflife/internal/store"
)

func deactivateUserGlobally(t *testing.T, env *integrationEnv, userID int64) {
	t.Helper()
	db, err := sql.Open("sqlite", env.srv.config.DatabasePath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(context.Background(), "UPDATE users SET is_active = 0 WHERE id = ?", userID)
	require.NoError(t, err)
}

func uploadMultipartBlock(
	t *testing.T, env *integrationEnv, cookie *http.Cookie,
	responseID int64, blockType, filename, contentType string, body []byte,
) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	require.NoError(t, mw.WriteField("kind", blockType))

	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="media"; filename=%q`, filename))
	hdr.Set("Content-Type", contentType)
	part, err := mw.CreatePart(hdr)
	require.NoError(t, err)
	_, err = part.Write(body)
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	csrfCookie, csrfToken := csrfPair()
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/responses/%d/blocks/upload", responseID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrfToken)
	req.AddCookie(cookie)
	req.AddCookie(csrfCookie)

	return env.do(t, req)
}

// TestSecurityRejectsSpoofedAudioVideoAndFFmpegPlaylist verifies that
// client-supplied Content-Type headers cannot smuggle an HLS/m3u8 playlist or
// arbitrary text file into ffmpeg via audio/video block uploads.
func TestSecurityRejectsSpoofedAudioVideoAndFFmpegPlaylist(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Maya", "maya@example.com")
	session := env.sessionCookie(t, user.ID)

	_, qIDs := env.seedIssue(t, "collecting", 8, 2026, 1)
	respID, err := env.store.CreateResponse(ctx, user.ID, qIDs[0])
	require.NoError(t, err)

	hlsPayload := []byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10.0,\nhttp://169.254.169.254/latest/meta-data/\n#EXT-X-ENDLIST\n")

	for _, blockType := range []string{"audio", "video"} {
		rr := uploadMultipartBlock(t, env, session, respID, blockType,
			"evil.webm", blockType+"/webm", hlsPayload)
		assert.Equal(t, http.StatusUnsupportedMediaType, rr.Code,
			"spoofed %s upload must be rejected: %s", blockType, rr.Body.String())
	}

	// Direct call to normalizeWebM on a non-EBML file must bail before ffmpeg.
	fakePath := filepath.Join(t.TempDir(), "fake.webm")
	require.NoError(t, os.WriteFile(fakePath, hlsPayload, 0o600))
	assert.False(t, hasEBMLHeader(fakePath))
	env.srv.normalizeWebM(ctx, fakePath)
	got, err := os.ReadFile(fakePath)
	require.NoError(t, err)
	assert.Equal(t, hlsPayload, got, "non-EBML file must be left untouched")

	// Valid Ogg magic bytes are accepted as audio/ogg.
	oggBytes := append([]byte("OggS"), make([]byte, 32)...)
	rr := uploadMultipartBlock(t, env, session, respID, "audio",
		"voice.ogg", "audio/ogg", oggBytes)
	assert.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
}

// TestSecurityPhotoStripsEXIFAndRejectsDecompressionBomb verifies user photo
// uploads strip EXIF/GPS metadata, apply orientation, and reject pixel bombs.
func TestSecurityPhotoStripsEXIFAndRejectsDecompressionBomb(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Noor", "noor@example.com")
	session := env.sessionCookie(t, user.ID)

	_, qIDs := env.seedIssue(t, "collecting", 8, 2026, 1)
	respID, err := env.store.CreateResponse(ctx, user.ID, qIDs[0])
	require.NoError(t, err)

	// Sideways JPEG carrying a GPS tag in EXIF.
	src := jpegWithOrientation(t, 200, 100, 6)
	require.Contains(t, string(src), "GPS-52.5N")

	rr := uploadMultipartBlock(t, env, session, respID, "photo", "holiday.jpg", "image/jpeg", src)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	blocks, err := env.store.ListBlocksByResponse(ctx, respID)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NotNil(t, blocks[0].FilePath)

	stored, err := os.ReadFile(*blocks[0].FilePath)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "GPS-52.5N", "EXIF/GPS metadata must be stripped")

	cfg, _, err := image.DecodeConfig(bytes.NewReader(stored))
	require.NoError(t, err)
	assert.Equal(t, 100, cfg.Width, "EXIF orientation 6 rotates 200x100 to 100x200")
	assert.Equal(t, 200, cfg.Height)

	// Minimal PNG header claiming 20000x20000 pixels (400M > 64M limit).
	bomb := makePNGHeader(20000, 20000)
	rrBomb := uploadMultipartBlock(t, env, session, respID, "photo", "bomb.png", "image/png", bomb)
	assert.Equal(t, http.StatusUnprocessableEntity, rrBomb.Code, rrBomb.Body.String())
}

// makePNGHeader constructs a minimal PNG signature + IHDR chunk claiming w×h.
func makePNGHeader(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var ihdr [13]byte
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // RGB
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)))
	buf.WriteString("IHDR")
	buf.Write(ihdr[:])
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("IHDR"))
	_, _ = crc.Write(ihdr[:])
	_ = binary.Write(&buf, binary.BigEndian, crc.Sum32())
	return buf.Bytes()
}

// TestSecurityLoopAdminCannotMutateOtherUsersOrReactivateGlobally verifies
// a Loop-scoped admin cannot edit another member's global profile/preferences
// or reactivate a globally deactivated account, whereas an instance admin can.
func TestSecurityLoopAdminCannotMutateOtherUsersOrReactivateGlobally(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	loopAdmin := env.createUserWithRole(t, "LoopAdmin", "loopadmin@example.com", "admin")
	require.NoError(t, env.store.SeedAdminUser(ctx, "instadmin@example.com"))
	instAdmin, err := env.store.GetUserByEmail(ctx, "instadmin@example.com")
	require.NoError(t, err)
	member := env.createUser(t, "Sam", "sam@example.com")
	require.NoError(t, env.store.UpsertNotificationPreferences(ctx, &store.NotificationPreferences{UserID: member.ID}))
	banned := env.createUser(t, "Banned", "banned@example.com")
	deactivateUserGlobally(t, env, banned.ID)

	call := func(actorID int64, method, path, body string) *httptest.ResponseRecorder {
		csrfCookie, csrfToken := csrfPair()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-CSRF-Token", csrfToken)
		req.AddCookie(env.sessionCookie(t, actorID))
		req.AddCookie(csrfCookie)
		return env.do(t, req)
	}

	// Loop admin blocked from editing Sam's profile or reading/writing Sam's preferences.
	assert.Equal(t, http.StatusForbidden,
		call(loopAdmin.ID, http.MethodPatch, fmt.Sprintf("/api/users/%d", member.ID), `{"name":"Hijacked"}`).Code)
	assert.Equal(t, http.StatusForbidden,
		call(loopAdmin.ID, http.MethodGet, fmt.Sprintf("/api/users/%d/preferences", member.ID), "").Code)
	assert.Equal(t, http.StatusForbidden,
		call(loopAdmin.ID, http.MethodPatch, fmt.Sprintf("/api/users/%d/preferences", member.ID), `{"issue_open":false}`).Code)

	// Instance admin CAN still manage Sam's profile and preferences.
	assert.Equal(t, http.StatusOK,
		call(instAdmin.ID, http.MethodPatch, fmt.Sprintf("/api/users/%d", member.ID), `{"name":"Samuel"}`).Code)
	assert.Equal(t, http.StatusOK,
		call(instAdmin.ID, http.MethodGet, fmt.Sprintf("/api/users/%d/preferences", member.ID), "").Code)

	// Loop admin CANNOT reactivate a globally deactivated account via invite.
	assert.Equal(t, http.StatusForbidden,
		call(loopAdmin.ID, http.MethodPost, "/api/users/invite", `{"email":"banned@example.com"}`).Code)
	stillBanned, err := env.store.GetUserByID(ctx, banned.ID)
	require.NoError(t, err)
	assert.False(t, stillBanned.IsActive)

	// Instance admin CAN reactivate via invite.
	rrReactivate := call(instAdmin.ID, http.MethodPost, "/api/users/invite", `{"email":"banned@example.com"}`)
	require.Equal(t, http.StatusOK, rrReactivate.Code, rrReactivate.Body.String())
	reactivated, err := env.store.GetUserByID(ctx, banned.ID)
	require.NoError(t, err)
	assert.True(t, reactivated.IsActive)
}

// TestSecurityUploadAuthorizationScoping verifies /uploads/ enforces strict
// ownership for private Rambles and active Loop membership + publication state
// for Loop media.
func TestSecurityUploadAuthorizationScoping(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	require.NoError(t, env.store.SeedAdminUser(ctx, "inst@example.com"))
	instAdmin, err := env.store.GetUserByEmail(ctx, "inst@example.com")
	require.NoError(t, err)
	loopAdmin := env.createUserWithRole(t, "LoopAdmin", "la@example.com", "admin")
	alice := env.createUser(t, "Alice", "alice@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	// Create a second Loop and a user belonging only to Loop 2.
	group2ID, err := env.store.CreateGroup(ctx, "Second Loop", &instAdmin.ID)
	require.NoError(t, err)
	outsiderID, err := env.store.CreateUser(ctx, "Outsider", "outsider@example.com")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, group2ID, outsiderID, "member"))

	writeUpload := func(rel, content string) (string, string) {
		full := filepath.Join(env.srv.config.UploadPath, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		return full, "/uploads/" + filepath.ToSlash(rel)
	}

	get := func(userID int64, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(env.sessionCookie(t, userID))
		return env.do(t, req)
	}

	// 1. Private Ramble photo: owner only (not Loop peer, not even instance admin).
	rambleFile, rambleURL := writeUpload("2026/08/ramble_secret.jpg", "ramble-bytes")
	dayID, err := env.store.EnsureRambleDay(ctx, alice.ID, "2026-08-01")
	require.NoError(t, err)
	_, err = env.store.CreateRambleBlock(ctx, dayID, "photo", nil, &rambleFile, nil)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, get(alice.ID, rambleURL).Code)
	assert.Equal(t, http.StatusNotFound, get(bob.ID, rambleURL).Code)
	assert.Equal(t, http.StatusNotFound, get(loopAdmin.ID, rambleURL).Code)
	assert.Equal(t, http.StatusNotFound, get(instAdmin.ID, rambleURL).Code,
		"private Ramble media must not be readable even by instance admins")

	// 2. Collecting-round response photo: author and Loop admin only until published.
	issueID, qIDs := env.seedIssue(t, "collecting", 8, 2026, 1)
	respID, err := env.store.CreateResponse(ctx, alice.ID, qIDs[0])
	require.NoError(t, err)
	respFile, respURL := writeUpload("2026/08/answer_photo.jpg", "answer-bytes")
	_, err = env.store.CreateBlock(ctx, respID, "photo", nil, &respFile, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, respID))

	assert.Equal(t, http.StatusOK, get(alice.ID, respURL).Code)
	assert.Equal(t, http.StatusOK, get(loopAdmin.ID, respURL).Code)
	assert.Equal(t, http.StatusNotFound, get(bob.ID, respURL).Code,
		"peers must not read another member's photo before issue publication")
	assert.Equal(t, http.StatusNotFound, get(outsiderID, respURL).Code)

	// Once published: active Loop 1 members can read it; Loop 2 outsider still cannot.
	require.NoError(t, env.store.PublishIssue(ctx, issueID))
	assert.Equal(t, http.StatusOK, get(bob.ID, respURL).Code)
	assert.Equal(t, http.StatusNotFound, get(outsiderID, respURL).Code,
		"users from another Loop must not access Loop 1 uploads")

	// After Bob is removed from Loop 1, Bob loses access immediately.
	require.NoError(t, env.store.SetMembershipActive(ctx, 1, bob.ID, false))
	assert.Equal(t, http.StatusNotFound, get(bob.ID, respURL).Code)
}

// TestSecurityCommentsAndDraftIssuesGated verifies comments cannot be posted
// or read on collecting issues or unsubmitted drafts, and draft issues are
// hidden from non-admin members.
func TestSecurityCommentsAndDraftIssuesGated(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	alice := env.createUser(t, "Alice", "alice@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	// Collecting issue with submitted response + dump item.
	collID, collQs := env.seedIssue(t, "collecting", 8, 2026, 1)
	collRespID, err := env.store.CreateResponse(ctx, alice.ID, collQs[0])
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, collRespID))
	dumpPath := filepath.Join(env.srv.config.UploadPath, "d.jpg")
	dumpID, err := env.store.CreateDumpItem(ctx, collID, alice.ID, "photo", nil, dumpPath, nil)
	require.NoError(t, err)

	// Published issue with an unsubmitted draft response.
	_, pubQs := env.seedIssue(t, "published", 7, 2026, 1)
	draftRespID, err := env.store.CreateResponse(ctx, alice.ID, pubQs[0])
	require.NoError(t, err)

	// Draft (upcoming) issue.
	draftIssueID, _ := env.seedIssue(t, "draft", 9, 2026, 1)

	postComment := func(actorID int64, path string) int {
		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(http.MethodPost, path, `{"body":"peek"}`)
		req.AddCookie(env.sessionCookie(t, actorID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		return env.do(t, req).Code
	}

	assert.Equal(t, http.StatusNotFound,
		postComment(bob.ID, fmt.Sprintf("/api/responses/%d/comments", collRespID)))
	assert.Equal(t, http.StatusNotFound,
		postComment(bob.ID, fmt.Sprintf("/api/dump/%d/comments", dumpID)))
	assert.Equal(t, http.StatusNotFound,
		postComment(bob.ID, fmt.Sprintf("/api/responses/%d/comments", draftRespID)))

	// Draft issue and its questions are hidden from regular members, visible to admins.
	for _, p := range []string{
		fmt.Sprintf("/api/issues/%d", draftIssueID),
		fmt.Sprintf("/api/issues/%d/questions", draftIssueID),
	} {
		reqMember := httptest.NewRequest(http.MethodGet, p, nil)
		reqMember.AddCookie(env.sessionCookie(t, bob.ID))
		assert.Equal(t, http.StatusNotFound, env.do(t, reqMember).Code, p)

		reqAdmin := httptest.NewRequest(http.MethodGet, p, nil)
		reqAdmin.AddCookie(env.sessionCookie(t, admin.ID))
		assert.Equal(t, http.StatusOK, env.do(t, reqAdmin).Code, p)
	}
}

// TestSecurityAuthTokenLogoutRevocationAndDeactivatedBlock verifies logout
// revokes outstanding email/login tokens, clicking a revoked link auto-resends
// a fresh login link (respecting the rate limit), and deactivated users can
// never exchange tokens or trigger auto-resends.
func TestSecurityAuthTokenLogoutRevocationAndDeactivatedBlock(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	alice := env.createUser(t, "Alice", "alice@example.com")
	session := env.sessionCookie(t, alice.ID)

	ctaRaw, ctaHash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAuthToken(
		ctx, alice.ID, ctaHash, "email_cta", time.Now().Add(7*24*time.Hour)))

	// Logout revokes outstanding auth tokens for Alice.
	csrfCookie, csrfToken := csrfPair()
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.AddCookie(session)
	logoutReq.AddCookie(csrfCookie)
	logoutReq.Header.Set("X-CSRF-Token", csrfToken)
	logoutRR := env.do(t, logoutReq)
	require.Equal(t, http.StatusNoContent, logoutRR.Code)

	// Clicking the pre-logout email_cta link while logged out must NOT mint a
	// session; it auto-sends a fresh login link and informs the user.
	rr := env.do(t, httptest.NewRequest(http.MethodGet, "/issues?auth="+ctaRaw, nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	assert.Equal(t, "/login?error=used_resent", rr.Header().Get("Location"))
	assert.Nil(t, findCookie(rr, "session"))

	// Rendering /login?error=used_resent displays the reassuring confirmation.
	page := env.do(t, httptest.NewRequest(http.MethodGet, "/login?error=used_resent", nil))
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), "already been used")
	assert.Contains(t, page.Body.String(), "sent a fresh link")

	// Deactivated user cannot redeem an unexpired token or trigger auto-resend.
	bob := env.createUser(t, "Bob", "bob@example.com")
	bobRaw, bobHash, err := auth.GenerateRandomToken(32)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAuthToken(
		ctx, bob.ID, bobHash, "login", time.Now().Add(30*time.Minute)))
	deactivateUserGlobally(t, env, bob.ID)

	bobRR := env.do(t, httptest.NewRequest(http.MethodGet, "/auth/verify?token="+bobRaw, nil))
	require.Equal(t, http.StatusSeeOther, bobRR.Code)
	assert.Equal(t, "/login?error=invalid", bobRR.Header().Get("Location"))
	assert.Nil(t, findCookie(bobRR, "session"))
}

// TestSecurityLoginErrorNoSpoofingAndStrictCSPNonce verifies arbitrary ?error=
// strings are never reflected on /login, and every page serves a per-request
// CSP nonce with no 'unsafe-inline' in script-src.
func TestSecurityLoginErrorNoSpoofingAndStrictCSPNonce(t *testing.T) {
	env := newIntegrationEnv(t)

	rr := env.do(t, httptest.NewRequest(http.MethodGet, "/login?error=CALL+555-0199+NOW", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.NotContains(t, body, "CALL 555-0199 NOW", "arbitrary error text must not be reflected")
	assert.Contains(t, body, "Unable to sign in with that link")

	csp := rr.Header().Get("Content-Security-Policy")
	re := regexp.MustCompile(`script-src 'self' 'nonce-([^']+)'`)
	m := re.FindStringSubmatch(csp)
	require.Len(t, m, 2, "expected nonce-based script-src in CSP, got: %s", csp)
	assert.NotContains(t, csp, "script-src 'self' 'unsafe-inline'")
	assert.Contains(t, body, fmt.Sprintf(`<script nonce="%s">`, m[1]))

	// Second request produces a distinct nonce.
	rr2 := env.do(t, httptest.NewRequest(http.MethodGet, "/login", nil))
	m2 := re.FindStringSubmatch(rr2.Header().Get("Content-Security-Policy"))
	require.Len(t, m2, 2)
	assert.NotEqual(t, m[1], m2[1], "CSP nonce must be freshly generated per request")
}

// TestSecurityURLValidationAndLoginIPRateLimit verifies javascript:/data: URLs
// are rejected on avatar_url and link blocks, and per-IP magic-link rate
// limiting stops spraying across distinct email addresses.
func TestSecurityURLValidationAndLoginIPRateLimit(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	user := env.createUser(t, "Dev", "dev@example.com")
	session := env.sessionCookie(t, user.ID)
	_, qIDs := env.seedIssue(t, "collecting", 8, 2026, 1)
	respID, err := env.store.CreateResponse(ctx, user.ID, qIDs[0])
	require.NoError(t, err)

	doJSON := func(method, path, body string) *httptest.ResponseRecorder {
		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(method, path, body)
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		return env.do(t, req)
	}

	// Reject dangerous avatar URLs, accept https:// and /uploads/.
	for _, bad := range []string{"javascript:alert(1)", "data:image/svg+xml,<svg/>", "//evil.example/a.png"} {
		payload, _ := json.Marshal(map[string]string{"avatar_url": bad})
		rr := doJSON(http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID), string(payload))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "avatar_url %q must be rejected", bad)
	}
	assert.Equal(t, http.StatusOK,
		doJSON(http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID), `{"avatar_url":"https://cdn.example.com/a.png"}`).Code)
	assert.Equal(t, http.StatusOK,
		doJSON(http.MethodPatch, fmt.Sprintf("/api/users/%d", user.ID), `{"avatar_url":"/uploads/2026/08/a.jpg"}`).Code)

	// Reject dangerous link block URLs, accept https://.
	for _, bad := range []string{"javascript:alert(1)", "data:text/html,<script>alert(1)</script>"} {
		payload, _ := json.Marshal(map[string]string{"type": "link", "link_url": bad})
		rr := doJSON(http.MethodPost, fmt.Sprintf("/api/responses/%d/blocks", respID), string(payload))
		assert.Equal(t, http.StatusBadRequest, rr.Code, "link_url %q must be rejected", bad)
	}
	assert.Equal(t, http.StatusCreated,
		doJSON(http.MethodPost, fmt.Sprintf("/api/responses/%d/blocks", respID), `{"type":"link","link_url":"https://example.com/post"}`).Code)

	// Per-IP rate limit blocks spraying 21 distinct addresses from one IP.
	csrfCookie, csrfToken := csrfPair()
	for i := 1; i <= 21; i++ {
		req := newJSONRequest(http.MethodPost, "/api/auth/login",
			fmt.Sprintf(`{"email":"spray%d@example.com"}`, i))
		req.RemoteAddr = "203.0.113.10:54321"
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		rr := env.do(t, req)
		if i <= 20 {
			assert.Equal(t, http.StatusOK, rr.Code, "attempt %d should succeed", i)
		} else {
			assert.Equal(t, http.StatusTooManyRequests, rr.Code, "21st distinct-email attempt from same IP must be rate-limited")
		}
	}

	// A different client IP is unaffected.
	reqOther := newJSONRequest(http.MethodPost, "/api/auth/login", `{"email":"fresh@example.com"}`)
	reqOther.RemoteAddr = "203.0.113.99:54321"
	reqOther.AddCookie(csrfCookie)
	reqOther.Header.Set("X-CSRF-Token", csrfToken)
	assert.Equal(t, http.StatusOK, env.do(t, reqOther).Code)
}
