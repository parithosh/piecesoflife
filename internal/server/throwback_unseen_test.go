package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestThrowbackPhotosOneYearAgoAndFallback verifies the "Photos from one year
// ago" closing section:
//   - picks 5 random uncovered photos from the published issue 1Y earlier
//     across answers, dump items, and notebooks, excluding covered & draft media
//   - falls back to the 12th earlier published issue when no exact 1Y issue exists
//   - omits the section when neither a 1Y issue nor 12 earlier issues exist.
func TestThrowbackPhotosOneYearAgoAndFallback(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	alice := env.createUser(t, "Alice", "alice@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	getIssuePage := func(year, month int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/issues/%d/%02d", year, month), nil)
		req.AddCookie(env.sessionCookie(t, alice.ID))
		rr := env.do(t, req)
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}

	makePublishedIssue := func(title string, year, month int) int64 {
		t.Helper()
		opens := time.Date(year, time.Month(month), 1, 9, 0, 0, 0, time.UTC)
		id, err := env.store.CreateIssue(ctx, 1, &title, month, year, opens, opens.Add(7*24*time.Hour))
		require.NoError(t, err)
		_, err = env.store.CreateQuestion(ctx, id, "How was the month?", nil, "admin", nil, 0)
		require.NoError(t, err)
		require.NoError(t, env.store.PublishIssue(ctx, id))
		return id
	}

	// 1. Exact 1Y match: Nov 2025 -> Nov 2026.
	oldID := makePublishedIssue("November 2025", 2025, 11)
	oldQs, err := env.store.ListQuestionsByIssue(ctx, oldID)
	require.NoError(t, err)

	// Add 4 uncovered response photos + 1 covered response photo + 1 draft photo.
	respID, err := env.store.CreateResponse(ctx, alice.ID, oldQs[0].ID)
	require.NoError(t, err)
	for i := 1; i <= 4; i++ {
		fp := fmt.Sprintf("/tmp/uploads/tb-resp-%d.jpg", i)
		cap := fmt.Sprintf("Response photo %d", i)
		_, bErr := env.store.CreateBlock(ctx, respID, "photo", nil, &fp, &cap, nil, i)
		require.NoError(t, bErr)
	}
	coveredPath := "/tmp/uploads/tb-covered.jpg"
	coveredBlockID, err := env.store.CreateBlock(ctx, respID, "photo", nil, &coveredPath, nil, nil, 5)
	require.NoError(t, err)
	require.NoError(t, env.store.UpdateBlockCover(ctx, coveredBlockID, true, nil))
	require.NoError(t, env.store.SubmitResponse(ctx, respID))

	// Draft photo must never leak into throwback.
	draftRespID, err := env.store.CreateResponse(ctx, bob.ID, oldQs[0].ID)
	require.NoError(t, err)
	draftPath := "/tmp/uploads/tb-draft.jpg"
	_, err = env.store.CreateBlock(ctx, draftRespID, "photo", nil, &draftPath, nil, nil, 0)
	require.NoError(t, err)

	// Add 2 uncovered dump photos + 1 covered dump photo.
	jpeg := "image/jpeg"
	for i := 1; i <= 2; i++ {
		cap := fmt.Sprintf("Dump photo %d", i)
		_, dErr := env.store.CreateDumpItem(ctx, oldID, bob.ID, "photo", &jpeg,
			fmt.Sprintf("/tmp/uploads/tb-dump-%d.jpg", i), &cap)
		require.NoError(t, dErr)
	}
	coveredDumpID, err := env.store.CreateDumpItem(ctx, oldID, bob.ID, "photo", &jpeg,
		"/tmp/uploads/tb-dump-covered.jpg", nil)
	require.NoError(t, err)
	require.NoError(t, env.store.UpdateDumpItemCover(ctx, coveredDumpID, true, nil))

	// Add 1 uncovered notebook photo (total 7 uncovered photos on Nov 2025).
	rambleID, err := env.store.EnsureRambleDay(ctx, alice.ID, "2025-11-03")
	require.NoError(t, err)
	diaryPhoto := "/tmp/uploads/tb-diary-1.jpg"
	_, err = env.store.CreateRambleBlock(ctx, rambleID, "photo", nil, &diaryPhoto, nil)
	require.NoError(t, err)
	_, _, err = env.store.AttachDiarySection(ctx, oldID, alice.ID, "2025-11-01", "2025-11-05")
	require.NoError(t, err)

	// Deterministic full-pool privacy check: covered and draft photos must
	// never appear anywhere in the throwback candidate pool.
	allPool, err := env.store.ListRandomThrowbackPhotos(ctx, oldID, 100)
	require.NoError(t, err)
	require.Len(t, allPool, 7, "expected all 7 uncovered published photos in pool")
	for _, p := range allPool {
		assert.NotContains(t, p.FilePath, "tb-covered.jpg", "covered response photo must be excluded from pool")
		assert.NotContains(t, p.FilePath, "tb-dump-covered.jpg", "covered dump photo must be excluded from pool")
		assert.NotContains(t, p.FilePath, "tb-draft.jpg", "draft photo must be excluded from pool")
	}

	// Current issue: Nov 2026.
	makePublishedIssue("November 2026", 2026, 11)

	body := getIssuePage(2026, 11)
	assert.Contains(t, body, "Photos from one year ago")
	assert.Contains(t, body, "November 2025")
	assert.Contains(t, body, `/issues/2025/11`)
	assert.Equal(t, 5, strings.Count(body, `class="pl-throwback-item"`),
		"must select exactly 5 photos when 7 uncovered photos exist")
	assert.NotContains(t, body, "tb-covered.jpg", "covered response photo must be excluded")
	assert.NotContains(t, body, "tb-dump-covered.jpg", "covered dump photo must be excluded")
	assert.NotContains(t, body, "tb-draft.jpg", "draft photo must be excluded")

	// Nov 2025 itself has no earlier issues -> throwback section must be absent.
	oldBody := getIssuePage(2025, 11)
	assert.NotContains(t, oldBody, "data-throwback-page")
	assert.NotContains(t, oldBody, "Photos from one year ago")

	// 2. Fallback to 12th earlier published issue when no exact 1Y match exists.
	// Use a fresh circle (Group 2) so issue counts are isolated.
	require.NoError(t, env.store.SeedAdminUser(ctx, "op@example.com"))
	op, err := env.store.GetUserByEmail(ctx, "op@example.com")
	require.NoError(t, err)
	group2ID, err := env.store.CreateGroup(ctx, "Fallback Circle", &op.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.CompleteSetup(ctx, group2ID))
	require.NoError(t, env.store.CreateMembership(ctx, group2ID, alice.ID, "member"))

	// Create 13 earlier published issues (months 1..12 of 2024 + Jan 2025),
	// then a target issue in March 2026 (no March 2025 exists!).
	// The 12th earlier issue before Mar 2026 is Feb 2024 (index 11 descending:
	// Jan 2025 [0], Dec 2024 [1], Nov [2], Oct [3], Sep [4], Aug [5], Jul [6],
	// Jun [7], May [8], Apr [9], Mar [10], Feb 2024 [11]).
	var feb2024ID int64
	for m := 1; m <= 12; m++ {
		title := fmt.Sprintf("2024-%02d", m)
		opens := time.Date(2024, time.Month(m), 1, 9, 0, 0, 0, time.UTC)
		id, cErr := env.store.CreateIssue(ctx, group2ID, &title, m, 2024, opens, opens.Add(7*24*time.Hour))
		require.NoError(t, cErr)
		require.NoError(t, env.store.PublishIssue(ctx, id))
		if m == 2 {
			feb2024ID = id
		}
	}
	jan25Title := "2025-01"
	jan25Opens := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	jan25ID, err := env.store.CreateIssue(ctx, group2ID, &jan25Title, 1, 2025, jan25Opens, jan25Opens.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, env.store.PublishIssue(ctx, jan25ID))

	// Put a recognizable photo on the 12th earlier issue (Feb 2024).
	fbCap := "Twelve issues back"
	_, err = env.store.CreateDumpItem(ctx, feb2024ID, alice.ID, "photo", &jpeg,
		"/tmp/uploads/fallback-12th.jpg", &fbCap)
	require.NoError(t, err)

	mar26Title := "March 2026"
	mar26Opens := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	mar26ID, err := env.store.CreateIssue(ctx, group2ID, &mar26Title, 3, 2026, mar26Opens, mar26Opens.Add(7*24*time.Hour))
	require.NoError(t, err)
	_, err = env.store.CreateQuestion(ctx, mar26ID, "Spring check-in?", nil, "admin", nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.PublishIssue(ctx, mar26ID))

	req := httptest.NewRequest(http.MethodGet, "/issues/2026/03", nil)
	req.AddCookie(env.sessionCookieForGroup(t, alice.ID, group2ID))
	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code)

	fbBody := rr.Body.String()
	assert.Contains(t, fbBody, "Photos from 12 issues ago")
	assert.NotContains(t, fbBody, "Photos from one year ago")
	assert.Contains(t, fbBody, "2024-02")
	assert.Contains(t, fbBody, "Twelve issues back")
	assert.Contains(t, fbBody, `/issues/2024/02`)
}

// TestUnseenCommentsTabVisibilityAndLifecycle verifies:
//   - issues published < 2 days ago never show the "Unseen comments" tab
//   - issues published >= 2 days ago show only comments from others with
//     activity in the last 2 days
//   - marking comments seen clears the unseen list until newer comments arrive
//   - cross-group mark-seen requests are rejected.
func TestUnseenCommentsTabVisibilityAndLifecycle(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	priya := env.createUser(t, "Priya", "priya@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	rawDB, err := sql.Open("sqlite", env.srv.config.DatabasePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rawDB.Close() })

	setPublishedAt := func(issueID int64, when time.Time) {
		t.Helper()
		_, err := rawDB.ExecContext(ctx,
			`UPDATE issues SET published_at = ? WHERE id = ?`,
			when.UTC().Format(time.DateTime), issueID)
		require.NoError(t, err)
	}

	setCommentCreatedAt := func(commentID int64, when time.Time) {
		t.Helper()
		_, err := rawDB.ExecContext(ctx,
			`UPDATE comments SET created_at = ? WHERE id = ?`,
			when.UTC().Format(time.DateTime), commentID)
		require.NoError(t, err)
	}

	getIssue := func(userID int64, year, month int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/issues/%d/%02d", year, month), nil)
		req.AddCookie(env.sessionCookie(t, userID))
		rr := env.do(t, req)
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}

	// 1. Fresh issue (< 2 days old): tab must NOT appear even with fresh comments.
	freshTitle := "Fresh Issue"
	freshOpens := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	freshID, err := env.store.CreateIssue(ctx, 1, &freshTitle, 11, 2026, freshOpens, freshOpens.Add(7*24*time.Hour))
	require.NoError(t, err)
	freshQID, err := env.store.CreateQuestion(ctx, freshID, "Favorite song?", nil, "admin", nil, 0)
	require.NoError(t, err)
	freshRespID, err := env.store.CreateResponse(ctx, bob.ID, freshQID)
	require.NoError(t, err)
	ans := "Anything acoustic."
	_, err = env.store.CreateBlock(ctx, freshRespID, "text", &ans, nil, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, freshRespID))
	require.NoError(t, env.store.PublishIssue(ctx, freshID))
	setPublishedAt(freshID, time.Now().UTC().Add(-12*time.Hour))

	_, err = env.store.CreateComment(ctx, priya.ID, freshRespID, nil, "Love this pick!")
	require.NoError(t, err)

	freshHTML := getIssue(bob.ID, 2026, 11)
	assert.NotContains(t, freshHTML, `data-read-mode="unseen"`,
		"issues under 2 days old must not show the Unseen comments tab")
	assert.NotContains(t, freshHTML, `id="unseen-comments-view"`)

	// 2. Mature issue (>= 2 days old): tab appears with only last-2D comments
	//    from other members.
	oldTitle := "Mature Issue"
	oldOpens := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	oldID, err := env.store.CreateIssue(ctx, 1, &oldTitle, 10, 2026, oldOpens, oldOpens.Add(7*24*time.Hour))
	require.NoError(t, err)
	oldQID, err := env.store.CreateQuestion(ctx, oldID, "Best meal this month?", nil, "admin", nil, 0)
	require.NoError(t, err)
	oldRespID, err := env.store.CreateResponse(ctx, bob.ID, oldQID)
	require.NoError(t, err)
	meal := "Homemade dosa on Sunday morning."
	_, err = env.store.CreateBlock(ctx, oldRespID, "text", &meal, nil, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, oldRespID))

	dumpCap := "Sunset from the terrace"
	jpeg := "image/jpeg"
	dumpID, err := env.store.CreateDumpItem(ctx, oldID, priya.ID, "photo", &jpeg,
		"/tmp/uploads/sunset.jpg", &dumpCap)
	require.NoError(t, err)

	// Covered dump photo with a secret caption: unseen tab must mask its
	// caption and omit the direct comments panel.
	secretCap := "Top secret surprise party photo"
	coveredDumpID, err := env.store.CreateDumpItem(ctx, oldID, priya.ID, "photo", &jpeg,
		"/tmp/uploads/secret-party.jpg", &secretCap)
	require.NoError(t, err)
	require.NoError(t, env.store.UpdateDumpItemCover(ctx, coveredDumpID, true, nil))

	// Notebook voice note with MIME type in Content: unseen tab must show
	// "Voice note", never "audio/webm".
	rambleID, err := env.store.EnsureRambleDay(ctx, priya.ID, "2026-10-05")
	require.NoError(t, err)
	audioMIME := "audio/webm"
	audioPath := "/tmp/uploads/voice.webm"
	_, err = env.store.CreateRambleBlock(ctx, rambleID, "audio", &audioMIME, &audioPath, nil)
	require.NoError(t, err)
	_, _, err = env.store.AttachDiarySection(ctx, oldID, priya.ID, "2026-10-01", "2026-10-10")
	require.NoError(t, err)
	diaryGroups, err := env.store.ListDiarySectionsByIssue(ctx, oldID)
	require.NoError(t, err)
	require.Len(t, diaryGroups, 1)
	audioBlockID := diaryGroups[0].Days[0].Blocks[0].ID

	require.NoError(t, env.store.PublishIssue(ctx, oldID))
	setPublishedAt(oldID, time.Now().UTC().Add(-5*24*time.Hour))

	// Stale comment from Priya (4 days ago -> older than 2D, excluded).
	staleCID, err := env.store.CreateComment(ctx, priya.ID, oldRespID, nil, "Old comment")
	require.NoError(t, err)
	setCommentCreatedAt(staleCID, time.Now().UTC().Add(-96*time.Hour))

	// Bob's own recent comment (1 hour ago -> own comment, excluded for Bob).
	ownCID, err := env.store.CreateComment(ctx, bob.ID, oldRespID, nil, "My own comment")
	require.NoError(t, err)
	setCommentCreatedAt(ownCID, time.Now().UTC().Add(-1*time.Hour))

	// Priya's recent comment on Bob's answer (6 hours ago -> UNSEEN).
	recentRespCID, err := env.store.CreateComment(ctx, priya.ID, oldRespID, nil, "Save me a dosa!")
	require.NoError(t, err)
	setCommentCreatedAt(recentRespCID, time.Now().UTC().Add(-6*time.Hour))

	// Priya's recent comment on uncovered dump item (2 hours ago -> UNSEEN).
	snapshotTime := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	recentDumpCID, err := env.store.CreateDumpComment(ctx, priya.ID, dumpID, nil, "Golden hour!")
	require.NoError(t, err)
	setCommentCreatedAt(recentDumpCID, snapshotTime)

	// Priya's recent comment on covered dump photo + audio diary block.
	coveredCID, err := env.store.CreateDumpComment(ctx, priya.ID, coveredDumpID, nil, "Shhh surprise!")
	require.NoError(t, err)
	setCommentCreatedAt(coveredCID, snapshotTime)
	audioCID, err := env.store.CreateDiaryBlockComment(ctx, priya.ID, audioBlockID, nil, "Loved hearing your voice!")
	require.NoError(t, err)
	setCommentCreatedAt(audioCID, snapshotTime)

	matureHTML := getIssue(bob.ID, 2026, 10)
	assert.Contains(t, matureHTML, `data-read-mode="unseen"`)
	assert.Contains(t, matureHTML, `data-unseen-count="4"`)
	assert.Contains(t, matureHTML, "Best meal this month?")
	assert.Contains(t, matureHTML, "Sunset from the terrace")

	// Extract unseen-comments-view slice to assert cover masking & voice-note label.
	unseenStart := strings.Index(matureHTML, `id="unseen-comments-view"`)
	require.Greater(t, unseenStart, 0)
	unseenSection := matureHTML[unseenStart:]
	assert.NotContains(t, unseenSection, secretCap, "covered photo caption must not appear in unseen tab")
	assert.NotContains(t, unseenSection, fmt.Sprintf(`data-dump-item-id="%d"`, coveredDumpID),
		"covered photo comments panel must not render directly in unseen tab")
	assert.Contains(t, unseenSection, "This comment is on a covered photo")
	assert.Contains(t, unseenSection, "Voice note")
	assert.NotContains(t, unseenSection, "audio/webm", "raw MIME type must not leak into unseen excerpt")

	// 3. Post a comment in the EXACT same second as snapshotTime (with a higher
	//    comment ID) before Bob marks the rendered snapshot seen. It must NOT
	//    be swallowed!
	sameSecCID, err := env.store.CreateComment(ctx, priya.ID, oldRespID, nil, "Arrived same second after render!")
	require.NoError(t, err)
	setCommentCreatedAt(sameSecCID, snapshotTime)

	csrfCookie, csrfToken := csrfPair()
	markBody := fmt.Sprintf(`{"as_of":%q,"max_comment_id":%d}`,
		snapshotTime.Format(time.RFC3339), audioCID)
	markReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/comments/mark-seen", oldID), strings.NewReader(markBody))
	markReq.AddCookie(env.sessionCookie(t, bob.ID))
	markReq.AddCookie(csrfCookie)
	markReq.Header.Set("X-CSRF-Token", csrfToken)
	markReq.Header.Set("Content-Type", "application/json")
	markRR := env.do(t, markReq)
	require.Equal(t, http.StatusNoContent, markRR.Code)

	// Same-second comment posted after render still shows as 1 unseen, and the
	// rendered panel carries data-unseen-after-id so the client matches it!
	afterSnapshotHTML := getIssue(bob.ID, 2026, 10)
	assert.Contains(t, afterSnapshotHTML, `data-unseen-count="1"`)
	assert.Contains(t, afterSnapshotHTML, fmt.Sprintf(`data-unseen-after-id="%d"`, audioCID))

	// Marking seen up through sameSecCID clears everything to 0.
	markBody2 := fmt.Sprintf(`{"as_of":%q,"max_comment_id":%d}`,
		snapshotTime.Format(time.RFC3339), sameSecCID)
	markReq2 := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/comments/mark-seen", oldID), strings.NewReader(markBody2))
	markReq2.AddCookie(env.sessionCookie(t, bob.ID))
	markReq2.AddCookie(csrfCookie)
	markReq2.Header.Set("X-CSRF-Token", csrfToken)
	markReq2.Header.Set("Content-Type", "application/json")
	require.Equal(t, http.StatusNoContent, env.do(t, markReq2).Code)

	// Monotonic check: an older tab posting an earlier as_of must NOT regress
	// the watermark backwards.
	staleMarkBody := fmt.Sprintf(`{"as_of":%q,"max_comment_id":%d}`,
		snapshotTime.Add(-1*time.Hour).Format(time.RFC3339), recentRespCID)
	staleMarkReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/comments/mark-seen", oldID), strings.NewReader(staleMarkBody))
	staleMarkReq.AddCookie(env.sessionCookie(t, bob.ID))
	staleMarkReq.AddCookie(csrfCookie)
	staleMarkReq.Header.Set("X-CSRF-Token", csrfToken)
	staleMarkReq.Header.Set("Content-Type", "application/json")
	require.Equal(t, http.StatusNoContent, env.do(t, staleMarkReq).Code)

	afterSeenHTML := getIssue(bob.ID, 2026, 10)
	assert.Contains(t, afterSeenHTML, `data-read-mode="unseen"`)
	assert.Contains(t, afterSeenHTML, `data-unseen-count="0"`)
	assert.Contains(t, afterSeenHTML, "all caught up")

	// 4. A brand-new comment posted AFTER mark-seen shows up as 1 unseen.
	brandNewCID, err := env.store.CreateComment(ctx, priya.ID, oldRespID, nil, "Coming over next week!")
	require.NoError(t, err)
	setCommentCreatedAt(brandNewCID, time.Now().UTC().Add(-1*time.Minute))

	newCommentHTML := getIssue(bob.ID, 2026, 10)
	assert.Contains(t, newCommentHTML, `data-unseen-count="1"`)

	// 5. Cross-group member cannot mark another circle's issue seen.
	require.NoError(t, env.store.SeedAdminUser(ctx, "op2@example.com"))
	op2, err := env.store.GetUserByEmail(ctx, "op2@example.com")
	require.NoError(t, err)
	otherGroupID, err := env.store.CreateGroup(ctx, "Other Circle", &op2.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.CompleteSetup(ctx, otherGroupID))
	outsiderID, err := env.store.CreateUser(ctx, "Outsider", "outsider@example.com")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, otherGroupID, outsiderID, "member"))

	crossReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/comments/mark-seen", oldID), nil)
	crossReq.AddCookie(env.sessionCookieForGroup(t, outsiderID, otherGroupID))
	crossReq.AddCookie(csrfCookie)
	crossReq.Header.Set("X-CSRF-Token", csrfToken)
	crossRR := env.do(t, crossReq)
	assert.Equal(t, http.StatusNotFound, crossRR.Code)
}

// TestLikesOnAnswersAndComments verifies liking/unliking published answers and
// comments, count/name persistence, draft rejection, and cross-group isolation.
func TestLikesOnAnswersAndComments(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	alice := env.createUser(t, "Alice", "alice@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	opens := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	title := "September 2026"
	issueID, err := env.store.CreateIssue(ctx, 1, &title, 9, 2026, opens, opens.Add(7*24*time.Hour))
	require.NoError(t, err)
	qID, err := env.store.CreateQuestion(ctx, issueID, "What made you smile?", nil, "admin", nil, 0)
	require.NoError(t, err)

	respID, err := env.store.CreateResponse(ctx, alice.ID, qID)
	require.NoError(t, err)
	ans := "Fresh mango season."
	_, err = env.store.CreateBlock(ctx, respID, "text", &ans, nil, nil, nil, 0)
	require.NoError(t, err)

	postLike := func(userID, groupID int64, path string) *httptest.ResponseRecorder {
		t.Helper()
		csrfCookie, csrfToken := csrfPair()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(env.sessionCookieForGroup(t, userID, groupID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		return env.do(t, req)
	}

	// Draft/unpublished response returns 404 (same as cross-Loop / missing)
	// so callers cannot probe for draft response IDs in their circle.
	draftRR := postLike(bob.ID, 1, fmt.Sprintf("/api/responses/%d/like", respID))
	assert.Equal(t, http.StatusNotFound, draftRR.Code)

	require.NoError(t, env.store.SubmitResponse(ctx, respID))
	require.NoError(t, env.store.PublishIssue(ctx, issueID))

	// Bob and Alice both like Alice's answer -> count 2.
	rr1 := postLike(bob.ID, 1, fmt.Sprintf("/api/responses/%d/like", respID))
	require.Equal(t, http.StatusOK, rr1.Code)
	var sum struct {
		Count int      `json:"count"`
		Liked bool     `json:"liked"`
		Names []string `json:"names"`
	}
	require.NoError(t, json.Unmarshal(rr1.Body.Bytes(), &sum))
	assert.True(t, sum.Liked)
	assert.Equal(t, 1, sum.Count)
	assert.Equal(t, []string{"Bob"}, sum.Names)

	rr2 := postLike(alice.ID, 1, fmt.Sprintf("/api/responses/%d/like", respID))
	require.Equal(t, http.StatusOK, rr2.Code)
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &sum))
	assert.True(t, sum.Liked)
	assert.Equal(t, 2, sum.Count)
	assert.Equal(t, []string{"Bob", "Alice"}, sum.Names)

	// Issue page renders the like counter and tooltip.
	pageReq := httptest.NewRequest(http.MethodGet, "/issues/2026/09", nil)
	pageReq.AddCookie(env.sessionCookie(t, bob.ID))
	pageRR := env.do(t, pageReq)
	require.Equal(t, http.StatusOK, pageRR.Code)
	assert.Contains(t, pageRR.Body.String(), fmt.Sprintf(`data-like-response="%d"`, respID))
	assert.Contains(t, pageRR.Body.String(), `data-like-count="2"`)
	assert.Contains(t, pageRR.Body.String(), `Liked by Bob, Alice`)

	// Bob unlikes -> count drops back to 1.
	rr3 := postLike(bob.ID, 1, fmt.Sprintf("/api/responses/%d/like", respID))
	require.Equal(t, http.StatusOK, rr3.Code)
	require.NoError(t, json.Unmarshal(rr3.Body.Bytes(), &sum))
	assert.False(t, sum.Liked)
	assert.Equal(t, 1, sum.Count)
	assert.Equal(t, []string{"Alice"}, sum.Names)

	// Comment liking + listing.
	commentID, err := env.store.CreateComment(ctx, bob.ID, respID, nil, "Same here!")
	require.NoError(t, err)

	crr := postLike(alice.ID, 1, fmt.Sprintf("/api/comments/%d/like", commentID))
	require.Equal(t, http.StatusOK, crr.Code)
	require.NoError(t, json.Unmarshal(crr.Body.Bytes(), &sum))
	assert.True(t, sum.Liked)
	assert.Equal(t, 1, sum.Count)

	listReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/responses/%d/comments", respID), nil)
	listReq.AddCookie(env.sessionCookie(t, alice.ID))
	listRR := env.do(t, listReq)
	require.Equal(t, http.StatusOK, listRR.Code)
	var listResp struct {
		Comments []struct {
			ID           int64    `json:"id"`
			LikeCount    int      `json:"like_count"`
			LikedByMe    bool     `json:"liked_by_me"`
			LikedByNames []string `json:"liked_by_names"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(listRR.Body.Bytes(), &listResp))
	require.Len(t, listResp.Comments, 1)
	assert.Equal(t, 1, listResp.Comments[0].LikeCount)
	assert.True(t, listResp.Comments[0].LikedByMe)
	assert.Equal(t, []string{"Alice"}, listResp.Comments[0].LikedByNames)

	// Cross-group member cannot like responses or comments in Loop 1.
	require.NoError(t, env.store.SeedAdminUser(ctx, "op3@example.com"))
	op3, err := env.store.GetUserByEmail(ctx, "op3@example.com")
	require.NoError(t, err)
	otherGroupID, err := env.store.CreateGroup(ctx, "Other Circle", &op3.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.CompleteSetup(ctx, otherGroupID))
	outsiderID, err := env.store.CreateUser(ctx, "Outsider", "outsider2@example.com")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, otherGroupID, outsiderID, "member"))

	assert.Equal(t, http.StatusNotFound,
		postLike(outsiderID, otherGroupID, fmt.Sprintf("/api/responses/%d/like", respID)).Code)
	assert.Equal(t, http.StatusNotFound,
		postLike(outsiderID, otherGroupID, fmt.Sprintf("/api/comments/%d/like", commentID)).Code)
}
