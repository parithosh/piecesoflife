package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

// TestDirectedQuestionsEndToEnd exercises the full @mention question lifecycle:
//   - suggestion submission from the landing page (archive) and validation of
//     target_user_id membership, self-targeting, and max length
//   - directed questions do not shrink the circle-wide QuestionsPerIssue target
//   - deactivated targets are pruned before opening
//   - only the targeted member sees (including via GET /api/issues/{id} and
//     emails) and may answer the directed prompt; non-admins don't see
//     submitted_by
//   - published issues aggregate answered directed Q&As into a single
//     "The hot seat" spread directly below Ramble ("From the notebooks"),
//     hiding unanswered directed prompts and preserving standard comments.
func TestDirectedQuestionsEndToEnd(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	require.NoError(t, env.store.SeedAdminUser(ctx, "operator@example.com"))
	operator, err := env.store.GetUserByEmail(ctx, "operator@example.com")
	require.NoError(t, err)

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	priya := env.createUser(t, "Priya", "priya@example.com")
	bob := env.createUser(t, "Bob", "bob@example.com")

	// Outsider in a separate Loop must not be targetable from Loop 1.
	group2ID, err := env.store.CreateGroup(ctx, "Other Circle", &operator.ID)
	require.NoError(t, err)
	outsiderID, err := env.store.CreateUser(ctx, "Outsider", "outsider@example.com")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, group2ID, outsiderID, "member"))

	// Seed a published issue + upcoming draft so the archive landing page
	// enters the "Coming up" state.
	settings, err := env.store.GetSettings(ctx, 1)
	require.NoError(t, err)
	settings.QuestionsPerIssue = 4
	require.NoError(t, env.store.UpdateSettings(ctx, settings))

	// Populate question bank so opening/preparing can fill the shared target.
	for i := 1; i <= 5; i++ {
		_, bErr := env.store.CreateBankQuestion(ctx, 1, fmt.Sprintf("Bank prompt %d?", i), "life_updates")
		require.NoError(t, bErr)
	}

	futureOpen := time.Now().UTC().Add(48 * time.Hour)
	futureClose := futureOpen.Add(7 * 24 * time.Hour)
	draftID, err := env.store.CreateIssue(ctx, 1, nil, int(futureOpen.Month()), futureOpen.Year(),
		futureOpen, futureClose)
	require.NoError(t, err)

	// 1. Archive landing page shows "Coming up" AND the suggestion box with
	//    active circle members (without leaking emails).
	getPage := func(userID int64, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(env.sessionCookie(t, userID))
		return env.do(t, req)
	}

	archiveRR := getPage(bob.ID, "/issues")
	require.Equal(t, http.StatusOK, archiveRR.Code)
	archiveBody := archiveRR.Body.String()
	assert.Contains(t, archiveBody, "Coming up")
	assert.Contains(t, archiveBody, "Suggest a question for the next issue")
	assert.Contains(t, archiveBody, `data-suggest-form`)
	assert.Contains(t, archiveBody, "Priya")
	assert.NotContains(t, archiveBody, "priya@example.com", "member emails must never appear in suggestion picker")

	postJSON := func(userID int64, path, body string) *httptest.ResponseRecorder {
		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(http.MethodPost, path, body)
		req.AddCookie(env.sessionCookie(t, userID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		return env.do(t, req)
	}

	// 2. Targeting an outsider is rejected with 400.
	badTarget := postJSON(bob.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":"Hey outsider?","target_user_id":%d}`, draftID, outsiderID,
	))
	assert.Equal(t, http.StatusBadRequest, badTarget.Code, badTarget.Body.String())

	// Self-targeting and over-500-char suggestions are rejected.
	selfTarget := postJSON(bob.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":"For myself?","target_user_id":%d}`, draftID, bob.ID,
	))
	assert.Equal(t, http.StatusBadRequest, selfTarget.Code)

	tooLong := postJSON(bob.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":%q}`, draftID, strings.Repeat("a", 501),
	))
	assert.Equal(t, http.StatusBadRequest, tooLong.Code)

	// Directed suggestion for a member who gets deactivated before prepare must
	// be pruned automatically so no orphaned hot-seat prompt survives.
	leaving := env.createUser(t, "Leaving", "leaving@example.com")
	rrLeaving := postJSON(bob.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":"Question for leaving member?","target_user_id":%d}`, draftID, leaving.ID,
	))
	require.Equal(t, http.StatusCreated, rrLeaving.Code, rrLeaving.Body.String())
	require.NoError(t, env.store.SetMembershipActive(ctx, 1, leaving.ID, false))

	// 3. Bob directs a question to Priya; Priya directs one to Admin (left unanswered).
	rrQ1 := postJSON(bob.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":"How was your Lisbon trip?","target_user_id":%d}`, draftID, priya.ID,
	))
	require.Equal(t, http.StatusCreated, rrQ1.Code, rrQ1.Body.String())
	var q1Resp struct {
		Question struct {
			ID             int64  `json:"id"`
			TargetUserID   *int64 `json:"target_user_id"`
			TargetUserName string `json:"target_user_name"`
		} `json:"question"`
	}
	require.NoError(t, json.Unmarshal(rrQ1.Body.Bytes(), &q1Resp))
	require.NotNil(t, q1Resp.Question.TargetUserID)
	assert.Equal(t, priya.ID, *q1Resp.Question.TargetUserID)
	assert.Equal(t, "Priya", q1Resp.Question.TargetUserName)
	priyaQID := q1Resp.Question.ID

	rrQ2 := postJSON(priya.ID, "/api/questions/submit", fmt.Sprintf(
		`{"issue_id":%d,"text":"Best coffee spot near you?","target_user_id":%d}`, draftID, admin.ID,
	))
	require.Equal(t, http.StatusCreated, rrQ2.Code, rrQ2.Body.String())

	// 4. Admin prepares the draft: leaving member's prompt is pruned, directed
	//    questions do NOT steal shared slots (4 shared + 2 active directed = 6),
	//    and curation closes the archive page's suggestion box.
	prepRR := postJSON(admin.ID, fmt.Sprintf("/api/issues/%d/questions/prepare", draftID), `{}`)
	require.Equal(t, http.StatusOK, prepRR.Code, prepRR.Body.String())

	allQs, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	sharedCount, directedCount := 0, 0
	var sharedQID int64
	for _, q := range allQs {
		assert.NotEqual(t, "Question for leaving member?", q.Text)
		if q.TargetUserID == nil {
			sharedCount++
			if sharedQID == 0 {
				sharedQID = q.ID
			}
		} else {
			directedCount++
		}
	}
	assert.Equal(t, 4, sharedCount, "directed questions must not shrink the circle-wide target (3 defaults + 1 bank)")
	assert.Equal(t, 2, directedCount)

	// Per-recipient email helper never leaks another member's directed prompt.
	priyaEmailQs := emailQuestionTextsForUser(allQs, priya.ID)
	assert.Contains(t, priyaEmailQs, "[Just for you] How was your Lisbon trip?")
	assert.NotContains(t, priyaEmailQs, "Best coffee spot near you?")
	assert.NotContains(t, priyaEmailQs, "[Just for you] Best coffee spot near you?")
	bobEmailQs := emailQuestionTextsForUser(allQs, bob.ID)
	assert.Len(t, bobEmailQs, 4)
	for _, txt := range bobEmailQs {
		assert.NotContains(t, txt, "Lisbon")
		assert.NotContains(t, txt, "coffee spot")
	}

	archiveAfterCurate := getPage(bob.ID, "/issues")
	require.Equal(t, http.StatusOK, archiveAfterCurate.Code)
	assert.Contains(t, archiveAfterCurate.Body.String(), "Coming up")
	assert.NotContains(t, archiveAfterCurate.Body.String(), `data-suggest-form`,
		"suggestion form closes once admin curates the upcoming draft")

	// 5. Flip the draft to collecting and check per-member visibility & guards.
	require.NoError(t, env.store.SetIssueStatus(ctx, draftID, "collecting"))

	// GET /api/issues/{id} hides other members' directed questions and strips submitted_by.
	bobIssueAPI := getPage(bob.ID, fmt.Sprintf("/api/issues/%d", draftID))
	require.Equal(t, http.StatusOK, bobIssueAPI.Code)
	assert.NotContains(t, bobIssueAPI.Body.String(), "How was your Lisbon trip?")
	assert.NotContains(t, bobIssueAPI.Body.String(), "Best coffee spot near you?")
	assert.NotContains(t, bobIssueAPI.Body.String(), fmt.Sprintf(`"submitted_by":%d`, bob.ID))

	priyaIssueAPI := getPage(priya.ID, fmt.Sprintf("/api/issues/%d", draftID))
	require.Equal(t, http.StatusOK, priyaIssueAPI.Code)
	assert.Contains(t, priyaIssueAPI.Body.String(), "How was your Lisbon trip?")
	assert.NotContains(t, priyaIssueAPI.Body.String(), "Best coffee spot near you?")
	assert.NotContains(t, priyaIssueAPI.Body.String(), fmt.Sprintf(`"submitted_by":%d`, bob.ID))

	// Priya sees the 4 shared prompts + her 1 directed prompt (5 total), NOT Admin's directed prompt.
	priyaRespond := getPage(priya.ID, fmt.Sprintf("/issues/%d/respond", draftID))
	require.Equal(t, http.StatusOK, priyaRespond.Code)
	priyaBody := priyaRespond.Body.String()
	assert.Contains(t, priyaBody, "How was your Lisbon trip?")
	assert.Contains(t, priyaBody, "The hot seat · just for you")
	assert.Contains(t, priyaBody, "5 prompts")
	assert.NotContains(t, priyaBody, "Best coffee spot near you?")

	// Bob sees only the 4 shared prompts (no directed prompts for him).
	bobRespond := getPage(bob.ID, fmt.Sprintf("/issues/%d/respond", draftID))
	require.Equal(t, http.StatusOK, bobRespond.Code)
	bobBody := bobRespond.Body.String()
	assert.Contains(t, bobBody, "4 prompts")
	assert.NotContains(t, bobBody, "How was your Lisbon trip?")
	assert.NotContains(t, bobBody, "Best coffee spot near you?")

	// API guard: Bob gets 404 trying to create a response to Priya's directed question.
	bobCreate := postJSON(bob.ID, "/api/responses", fmt.Sprintf(`{"question_id":%d}`, priyaQID))
	assert.Equal(t, http.StatusNotFound, bobCreate.Code)

	// Priya CAN answer her directed question and a shared question.
	priyaCreate := postJSON(priya.ID, "/api/responses", fmt.Sprintf(`{"question_id":%d}`, priyaQID))
	require.Equal(t, http.StatusCreated, priyaCreate.Code, priyaCreate.Body.String())
	var createdResp struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(priyaCreate.Body.Bytes(), &createdResp))
	priyaAnswer := "Pastel de nata every morning by the river."
	_, err = env.store.CreateBlock(ctx, createdResp.ID, "text", &priyaAnswer, nil, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, createdResp.ID))

	// Bob answers a shared question and attaches a Ramble diary so we verify
	// The hot seat renders directly below Ramble ("From the notebooks").
	bobRespID, err := env.store.CreateResponse(ctx, bob.ID, sharedQID)
	require.NoError(t, err)
	bobAnswer := "Great month all around."
	_, err = env.store.CreateBlock(ctx, bobRespID, "text", &bobAnswer, nil, nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, bobRespID))

	dayID, err := env.store.EnsureRambleDay(ctx, bob.ID, "2026-08-01")
	require.NoError(t, err)
	rambleNote := "Walked through the park."
	_, err = env.store.CreateRambleBlock(ctx, dayID, "text", &rambleNote, nil, nil)
	require.NoError(t, err)
	_, _, err = env.store.AttachDiarySection(ctx, draftID, bob.ID, "2026-08-01", "2026-08-02")
	require.NoError(t, err)

	// 6. Publish the issue and verify "The hot seat" spread appears
	//    right below Ramble with Priya's answer and its comment panel, and Bob
	//    can comment on Priya's directed answer.
	require.NoError(t, env.store.PublishIssue(ctx, draftID))

	commentRR := postJSON(bob.ID, fmt.Sprintf("/api/responses/%d/comments", createdResp.ID),
		`{"body":"Save me one next time!"}`)
	require.Equal(t, http.StatusCreated, commentRR.Code, commentRR.Body.String())

	pubURL := fmt.Sprintf("/issues/%d/%02d", futureOpen.Year(), int(futureOpen.Month()))
	pubRR := getPage(bob.ID, pubURL)
	require.Equal(t, http.StatusOK, pubRR.Code)
	pubBody := pubRR.Body.String()

	// Both directed questions live in the single "The hot seat" section below
	// Ramble ("From the notebooks") — NOT as standalone regular question pages.
	notebooksPos := strings.Index(pubBody, "data-diary-page")
	peoplesPos := strings.Index(pubBody, "data-peoples-choice-page")
	require.Greater(t, notebooksPos, 0, "expected notebooks section")
	require.Greater(t, peoplesPos, notebooksPos, "The hot seat must appear below Ramble (From the notebooks)")

	assert.Contains(t, pubBody, "The hot seat")
	assert.Contains(t, pubBody, "How was your Lisbon trip?")
	assert.NotContains(t, pubBody, "Best coffee spot near you?",
		"unanswered directed questions must not be published in The hot seat")
	assert.Contains(t, pubBody, "Pastel de nata every morning by the river.")
	assert.Contains(t, pubBody, fmt.Sprintf(`data-response-id="%d"`, createdResp.ID))
	assert.Contains(t, pubBody, `data-comment-count="1"`, "comments panel on directed answer shows comment count")
}

// TestAnswerTogetherEndToEnd: a member put in the hot seat twice about the
// same thing answers both questions at once — one card while answering, one
// answer under both questions once published — and only they can do it.
func TestAnswerTogetherEndToEnd(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	ashwin := env.createUser(t, "Ashwin", "ashwin@example.com")
	shrisha := env.createUser(t, "Shrisha", "shrisha@example.com")
	pari := env.createUser(t, "Pari", "pari@example.com")

	opens := time.Now().UTC().Add(48 * time.Hour)
	issueID, err := env.store.CreateIssue(ctx, 1, nil, int(opens.Month()), opens.Year(), opens, opens.Add(7*24*time.Hour))
	require.NoError(t, err)
	const toys = "Ashwin how was your birthday? Did you 3D print more toys?"
	const megha = "Ashwin how was your birthday? Did you celebrate it with Megha?"
	q1, err := env.store.CreateSuggestedQuestion(ctx, 1, issueID, shrisha.ID, toys, nil, &ashwin.ID)
	require.NoError(t, err)
	q2, err := env.store.CreateSuggestedQuestion(ctx, 1, issueID, pari.ID, megha, nil, &ashwin.ID)
	require.NoError(t, err)
	require.NoError(t, env.store.SetIssueStatus(ctx, issueID, "collecting"))

	call := func(userID int64, method, path, body string) *httptest.ResponseRecorder {
		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(method, path, body)
		req.AddCookie(env.sessionCookie(t, userID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)
		return env.do(t, req)
	}
	respondPage := func() string {
		rr := call(ashwin.ID, http.MethodGet, fmt.Sprintf("/issues/%d/respond", issueID), "")
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}
	together := func(userID int64, ids ...int64) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string][]int64{"question_ids": ids})
		return call(userID, http.MethodPost, "/api/questions/answer-together", string(body))
	}

	// Two separate cards, a "Group questions" control, and a tick box on
	// each hot-seat card — none on the circle-wide question.
	page := respondPage()
	assert.Contains(t, page, "data-group-start>Group questions</button>")
	assert.Equal(t, 2, strings.Count(page, `data-group-pick value=`))
	assert.Contains(t, page, fmt.Sprintf(`data-group-pick value="%d"`, q2))

	// Only Ashwin can combine his questions.
	assert.Equal(t, http.StatusNotFound, together(shrisha.ID, q1, q2).Code)
	assert.Equal(t, http.StatusBadRequest, together(ashwin.ID, q1).Code, "a group needs two")

	rr := together(ashwin.ID, q2, q1)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.JSONEq(t, fmt.Sprintf(`{"primary_id":%d}`, q1), rr.Body.String())

	// One card holding both prompts, offering to separate the second; with
	// nothing left to group, no grouping control. A stale tab can't start a
	// second answer for the joined question.
	page = respondPage()
	assert.Equal(t, 1, strings.Count(page, `class="pl-q pl-q--directed question-section"`))
	assert.NotContains(t, page, "data-group-start>Group questions</button>")
	assert.Contains(t, page, megha)
	assert.Contains(t, page, fmt.Sprintf(`data-answer-separately="%d"`, q2))
	assert.Equal(t, http.StatusConflict,
		call(ashwin.ID, http.MethodPost, "/api/responses", fmt.Sprintf(`{"question_id":%d}`, q2)).Code)
	assert.Equal(t, http.StatusConflict, together(ashwin.ID, q2, q1).Code, "already together: stale page")

	// The combined card has somewhere to type even though the primary's
	// answer row (created on submit, say) has no text block yet.
	blank := call(ashwin.ID, http.MethodPost, "/api/responses", fmt.Sprintf(`{"question_id":%d}`, q1))
	require.Equal(t, http.StatusCreated, blank.Code, blank.Body.String())
	page = respondPage()
	card := page[strings.Index(page, `class="pl-q pl-q--directed question-section"`):]
	assert.Contains(t, card[:strings.Index(card, "pl-q-actions")], `class="response-text"`)

	// Ashwin answers once.
	var resp struct{ ID int64 }
	require.NoError(t, json.Unmarshal(blank.Body.Bytes(), &resp))
	text := "A dragon, and Megha baked a cake."
	_, err = env.store.AutosaveResponse(ctx, resp.ID, 1, []store.ResponseBlock{{Type: "text", Content: &text}})
	require.NoError(t, err)
	require.NoError(t, env.store.SubmitResponse(ctx, resp.ID))

	// The admin can't remove the joined question: it has been answered.
	assert.Equal(t, http.StatusConflict,
		call(admin.ID, http.MethodDelete, fmt.Sprintf("/api/questions/%d", q2), "").Code)

	// Published: both questions once, stacked over the one answer.
	require.NoError(t, env.store.PublishIssue(ctx, issueID))
	rr = call(pari.ID, http.MethodGet, fmt.Sprintf("/issues/%d/%02d", opens.Year(), int(opens.Month())), "")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	assert.Equal(t, 1, strings.Count(body, `class="pl-peoples-q-line">`+template.HTMLEscapeString(megha)))
	assert.Equal(t, 1, strings.Count(body, `class="pl-peoples-q-line">`+template.HTMLEscapeString(toys)))
	assert.Equal(t, 1, strings.Count(body, "A dragon, and Megha baked a cake."))
	assert.Equal(t, 1, strings.Count(body, `class="pl-peoples-item`))
	assert.NotContains(t, body, "Shrisha", "askers stay anonymous")
}
