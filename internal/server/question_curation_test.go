package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpcomingIssueQuestionCuration(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	member := env.createUser(t, "Member", "member@example.com")
	adminSession := env.sessionCookie(t, admin.ID)
	memberSession := env.sessionCookie(t, member.ID)
	adminCSRF, adminCSRFHeader := csrfPair()
	memberCSRF, memberCSRFHeader := csrfPair()

	authed := func(
		req *http.Request, session, csrf *http.Cookie, csrfHeader string,
	) *http.Request {
		req.AddCookie(session)
		req.AddCookie(csrf)
		req.Header.Set("X-CSRF-Token", csrfHeader)
		return req
	}

	for i := range 10 {
		_, err := env.store.CreateBankQuestion(
			ctx, 1, fmt.Sprintf("Curation bank question %d?", i+1), "fun_silly",
		)
		require.NoError(t, err, "seed question bank")
	}

	publishedID, _ := env.seedIssue(t, "published", 1, 2025, 1)
	require.NotZero(t, publishedID)

	now := time.Now().UTC()
	draftID, err := env.store.CreateIssue(
		ctx, 1, nil, 9, 2027, now.Add(30*24*time.Hour), now.Add(37*24*time.Hour),
	)
	require.NoError(t, err, "create upcoming draft")

	suggestionBody := fmt.Sprintf(
		`{"issue_id":%d,"text":"What should we carry into next month?"}`, draftID,
	)
	suggestRR := env.do(t, authed(
		newJSONRequest(http.MethodPost, "/api/questions/submit", suggestionBody),
		memberSession, memberCSRF, memberCSRFHeader,
	))
	require.Equal(t, http.StatusCreated, suggestRR.Code, suggestRR.Body.String())

	memberIssueReq := httptest.NewRequest(http.MethodGet, "/issues/2025/1", nil)
	memberIssueReq.AddCookie(memberSession)
	memberIssueBefore := env.do(t, memberIssueReq)
	require.Equal(t, http.StatusOK, memberIssueBefore.Code)
	assert.Contains(t, memberIssueBefore.Body.String(), `id="next-question-form"`,
		"members can suggest while the draft is uncurated")

	adminDashboardReq := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminDashboardReq.AddCookie(adminSession)
	adminBefore := env.do(t, adminDashboardReq)
	require.Equal(t, http.StatusOK, adminBefore.Code)
	assert.Contains(t, adminBefore.Body.String(), "What should we carry into next month?")
	assert.Contains(t, adminBefore.Body.String(), "Prepare question set")

	preparePath := fmt.Sprintf("/api/issues/%d/questions/prepare", draftID)
	memberPrepare := env.do(t, authed(
		httptest.NewRequest(http.MethodPost, preparePath, nil),
		memberSession, memberCSRF, memberCSRFHeader,
	))
	assert.Equal(t, http.StatusForbidden, memberPrepare.Code,
		"only Loop admins may prepare the question set")

	prepareRR := env.do(t, authed(
		httptest.NewRequest(http.MethodPost, preparePath, nil),
		adminSession, adminCSRF, adminCSRFHeader,
	))
	require.Equal(t, http.StatusOK, prepareRR.Code, prepareRR.Body.String())

	draft, err := env.store.GetIssueByID(ctx, draftID)
	require.NoError(t, err)
	require.NotNil(t, draft.QuestionsCuratedAt, "preparation freezes the question set")

	prepared, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	require.Len(t, prepared, 6, "suggestion + defaults + bank fill the configured target")
	assert.Equal(t, "friend", prepared[0].Source, "existing suggestions stay first")

	counts := make(map[string]int, 4)
	preparedIDs := make([]int64, 0, len(prepared))
	for _, question := range prepared {
		counts[question.Source]++
		preparedIDs = append(preparedIDs, question.ID)
	}
	assert.Equal(t, 1, counts["friend"])
	assert.Equal(t, 3, counts["default"])
	assert.Equal(t, 2, counts["bank"])

	repeatRR := env.do(t, authed(
		httptest.NewRequest(http.MethodPost, preparePath, nil),
		adminSession, adminCSRF, adminCSRFHeader,
	))
	require.Equal(t, http.StatusOK, repeatRR.Code, repeatRR.Body.String())

	repeated, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	repeatedIDs := make([]int64, 0, len(repeated))
	for _, question := range repeated {
		repeatedIDs = append(repeatedIDs, question.ID)
	}
	assert.Equal(t, preparedIDs, repeatedIDs, "preparation is idempotent")

	lateSuggestion := env.do(t, authed(
		newJSONRequest(http.MethodPost, "/api/questions/submit",
			fmt.Sprintf(`{"issue_id":%d,"text":"An unreviewed late question?"}`, draftID)),
		memberSession, memberCSRF, memberCSRFHeader,
	))
	assert.Equal(t, http.StatusConflict, lateSuggestion.Code,
		"curation closes member suggestions")

	memberIssueReq = httptest.NewRequest(http.MethodGet, "/issues/2025/1", nil)
	memberIssueReq.AddCookie(memberSession)
	memberIssueAfter := env.do(t, memberIssueReq)
	require.Equal(t, http.StatusOK, memberIssueAfter.Code)
	assert.NotContains(t, memberIssueAfter.Body.String(), `id="next-question-form"`,
		"the closed suggestion form is no longer advertised")

	adminDashboardReq = httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminDashboardReq.AddCookie(adminSession)
	adminAfter := env.do(t, adminDashboardReq)
	require.Equal(t, http.StatusOK, adminAfter.Code)
	assert.Contains(t, adminAfter.Body.String(), "Questions ready for this issue")
	assert.Contains(t, adminAfter.Body.String(), "member suggestion")
	assert.NotContains(t, adminAfter.Body.String(), "Prepare question set")

	friendID := prepared[0].ID
	editRR := env.do(t, authed(
		newJSONRequest(http.MethodPatch, fmt.Sprintf("/api/questions/%d", friendID),
			`{"text":"What belongs in our next chapter?"}`),
		adminSession, adminCSRF, adminCSRFHeader,
	))
	require.Equal(t, http.StatusOK, editRR.Code, editRR.Body.String())

	var defaultID int64
	for _, question := range prepared {
		if question.Source == "default" {
			defaultID = question.ID
			break
		}
	}
	require.NotZero(t, defaultID)
	deleteRR := env.do(t, authed(
		httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/questions/%d", defaultID), nil),
		adminSession, adminCSRF, adminCSRFHeader,
	))
	require.Equal(t, http.StatusNoContent, deleteRR.Code, deleteRR.Body.String())

	curated, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	reversedIDs := make([]string, len(curated))
	for i, question := range curated {
		reversedIDs[len(curated)-1-i] = strconv.FormatInt(question.ID, 10)
	}
	reorderRR := env.do(t, authed(
		newJSONRequest(http.MethodPost,
			fmt.Sprintf("/api/issues/%d/questions/reorder", draftID),
			fmt.Sprintf(`{"question_ids":[%s]}`, strings.Join(reversedIDs, ","))),
		adminSession, adminCSRF, adminCSRFHeader,
	))
	require.Equal(t, http.StatusOK, reorderRR.Code, reorderRR.Body.String())

	want, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	wantIDs := make([]int64, 0, len(want))
	wantTexts := make([]string, 0, len(want))
	for _, question := range want {
		wantIDs = append(wantIDs, question.ID)
		wantTexts = append(wantTexts, question.Text)
	}
	require.Len(t, want, 5, "admin removed one question from the prepared set")

	require.NoError(t, env.srv.CreateNextIssue(ctx, 1, draft.OpensAt),
		"open curated draft on schedule")
	opened, err := env.store.GetIssueByID(ctx, draftID)
	require.NoError(t, err)
	assert.Equal(t, "collecting", opened.Status)

	got, err := env.store.ListQuestionsByIssue(ctx, draftID)
	require.NoError(t, err)
	gotIDs := make([]int64, 0, len(got))
	gotTexts := make([]string, 0, len(got))
	for _, question := range got {
		gotIDs = append(gotIDs, question.ID)
		gotTexts = append(gotTexts, question.Text)
	}
	assert.Equal(t, wantIDs, gotIDs,
		"scheduled opening preserves the admin's exact ordered set")
	assert.Equal(t, wantTexts, gotTexts)
	assert.Contains(t, gotTexts, "What belongs in our next chapter?")
}
