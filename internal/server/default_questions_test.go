package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

// countBySource tallies an issue's questions by their source column.
func countBySource(questions []store.Question) map[string]int {
	counts := make(map[string]int, 4)
	for _, q := range questions {
		counts[q.Source]++
	}

	return counts
}

// TestDefaultQuestionsAndBankFill covers the question fill rules: the target
// is the TOTAL question count per issue — enabled default questions and
// friend suggestions count toward it, the bank only pads the shortfall, and
// suggestions beyond the target are welcome (no padding, no trimming). The
// admin can switch defaults off globally or per question.
func TestDefaultQuestionsAndBankFill(t *testing.T) {
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

	for i := 0; i < 12; i++ {
		_, err := env.store.CreateBankQuestion(ctx, 1,
			fmt.Sprintf("Bank question %d?", i+1), "fun_silly")
		require.NoError(t, err, "seed bank question")
	}

	// The migration ships the three defaults, all enabled.
	defaults, err := env.store.ListDefaultQuestions(ctx, 1)
	require.NoError(t, err)
	require.Len(t, defaults, 3, "migration seeds three default questions")
	for _, dq := range defaults {
		assert.True(t, dq.Enabled, "defaults start enabled: %s", dq.Text)
	}

	// A fresh issue with the seeded target of 6: the 3 defaults count toward
	// it, so the bank supplies 3.
	createRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Round one"}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	first, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	questions, err := env.store.ListQuestionsByIssue(ctx, first.ID)
	require.NoError(t, err)
	counts := countBySource(questions)
	assert.Equal(t, 3, counts["default"], "all defaults stitched in")
	assert.Equal(t, 3, counts["bank"], "bank pads 6-target minus 3 defaults")
	assert.Len(t, questions, 6, "total hits the target exactly")
	assert.Equal(t, "default", questions[0].Source, "defaults lead a fresh issue")

	// Publishing pre-creates the next draft (auto-create is seeded on).
	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", first.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	draft, err := env.store.GetUpcomingDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft, "publish pre-creates the next draft")

	// One friend suggestion lands in the draft. It counts toward the target
	// of 6 alongside the 3 defaults, so the bank supplies exactly 2.
	suggest := func(issueID int64, n int) {
		for i := 0; i < n; i++ {
			body := strings.NewReader(fmt.Sprintf(
				`{"issue_id": %d, "text": "Friend suggestion %d for issue %d?"}`,
				issueID, i+1, issueID))
			req := authed(httptest.NewRequest(http.MethodPost, "/api/questions/submit", body))
			req.Header.Set("Content-Type", "application/json")
			require.Equal(t, http.StatusCreated, env.do(t, req).Code, "suggest question")
		}
	}
	suggest(draft.ID, 1)

	require.NoError(t, env.srv.CreateNextIssue(ctx, 1), "open the draft")

	questions, err = env.store.ListQuestionsByIssue(ctx, draft.ID)
	require.NoError(t, err)
	counts = countBySource(questions)
	assert.Equal(t, "friend", questions[0].Source, "suggestions stay first")
	assert.Equal(t, 1, counts["friend"])
	assert.Equal(t, 3, counts["default"])
	assert.Equal(t, 2, counts["bank"], "3 defaults + 1 friend leave room for 2 bank")
	assert.Len(t, questions, 6, "total hits the target exactly")

	// Globally disable all defaults, re-enable just one, and shrink the
	// target to 3 — then let suggestions overshoot it: 1 default + 3 friends
	// already exceed 3, so the bank adds nothing and nothing is trimmed.
	allOffRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		"/api/default-questions", `{"enabled": false}`)))
	require.Equal(t, http.StatusOK, allOffRR.Code, "disable all: %s", allOffRR.Body.String())

	oneOnRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/default-questions/%d", defaults[0].ID), `{"enabled": true}`)))
	require.Equal(t, http.StatusOK, oneOnRR.Code, "re-enable one: %s", oneOnRR.Body.String())

	settingsRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		"/api/admin/settings", `{"questions_per_issue": 3}`)))
	require.Equal(t, http.StatusOK, settingsRR.Code, "settings: %s", settingsRR.Body.String())

	pubRR = env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", draft.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish round two: %s", pubRR.Body.String())

	nextDraft, err := env.store.GetUpcomingDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, nextDraft)
	suggest(nextDraft.ID, 3)

	require.NoError(t, env.srv.CreateNextIssue(ctx, 1), "open round three")

	questions, err = env.store.ListQuestionsByIssue(ctx, nextDraft.ID)
	require.NoError(t, err)
	counts = countBySource(questions)
	assert.Equal(t, 1, counts["default"], "only the re-enabled default remains")
	assert.Equal(t, 3, counts["friend"])
	assert.Equal(t, 0, counts["bank"], "suggestions past the target mean no padding")
	assert.Len(t, questions, 4, "overshoot is kept, not trimmed")
	for _, q := range questions {
		if q.Source == "default" {
			assert.Equal(t, defaults[0].Text, q.Text)
		}
	}

	// A target beyond the cap is rejected.
	badRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		"/api/admin/settings", `{"questions_per_issue": 40}`)))
	assert.Equal(t, http.StatusBadRequest, badRR.Code, "cap enforced")
}

// TestManageDefaultQuestions covers the admin's control over the default
// set: adding custom prompts, rewording, reordering, and deleting — plus
// the guards (duplicates, stale reorder lists, unknown IDs).
func TestManageDefaultQuestions(t *testing.T) {
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

	// Add a custom default — it lands at the end, enabled.
	addRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/default-questions", `{"text": "What made you laugh this month?"}`)))
	require.Equal(t, http.StatusCreated, addRR.Code, "add: %s", addRR.Body.String())

	defaults, err := env.store.ListDefaultQuestions(ctx, 1)
	require.NoError(t, err)
	require.Len(t, defaults, 4, "custom default joins the seeded three")
	custom := defaults[3]
	assert.Equal(t, "What made you laugh this month?", custom.Text)
	assert.True(t, custom.Enabled, "custom defaults start enabled")

	// Duplicates are a validation problem, not a server error.
	dupRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/default-questions", `{"text": "What made you laugh this month?"}`)))
	assert.Equal(t, http.StatusBadRequest, dupRR.Code, "duplicate rejected")

	// Reword the custom default.
	editRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/default-questions/%d", custom.ID),
		`{"text": "What made you laugh out loud?"}`)))
	require.Equal(t, http.StatusOK, editRR.Code, "reword: %s", editRR.Body.String())

	// Rewording to another default's text is a validation error, not a 500.
	dupEditRR := env.do(t, authed(newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/default-questions/%d", custom.ID),
		fmt.Sprintf(`{"text": %q}`, defaults[0].Text))))
	assert.Equal(t, http.StatusBadRequest, dupEditRR.Code,
		"duplicate reword: %s", dupEditRR.Body.String())

	// Reorder: move the custom default to the front.
	ids := []string{fmt.Sprintf("%d", custom.ID)}
	for _, dq := range defaults[:3] {
		ids = append(ids, fmt.Sprintf("%d", dq.ID))
	}
	reorderRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/default-questions/reorder",
		fmt.Sprintf(`{"ids": [%s]}`, strings.Join(ids, ",")))))
	require.Equal(t, http.StatusOK, reorderRR.Code, "reorder: %s", reorderRR.Body.String())

	defaults, err = env.store.ListDefaultQuestions(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, custom.ID, defaults[0].ID, "custom default now leads")
	assert.Equal(t, "What made you laugh out loud?", defaults[0].Text, "reword persisted")

	// A stale reorder list (missing an ID) is rejected whole, and so is a
	// list padding the count with a duplicate ID.
	staleRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/default-questions/reorder",
		fmt.Sprintf(`{"ids": [%d]}`, custom.ID))))
	assert.Equal(t, http.StatusConflict, staleRR.Code, "partial order rejected")

	dupRR2 := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/default-questions/reorder",
		fmt.Sprintf(`{"ids": [%d, %d, %d, %d]}`,
			custom.ID, custom.ID, defaults[0].ID, defaults[1].ID))))
	assert.Equal(t, http.StatusConflict, dupRR2.Code, "duplicate IDs rejected")

	// A fresh issue stitches the custom default in first.
	createRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Round one"}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	issue, err := env.store.GetActiveIssue(ctx, 1)
	require.NoError(t, err)

	questions, err := env.store.ListQuestionsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.NotEmpty(t, questions)
	assert.Equal(t, "What made you laugh out loud?", questions[0].Text,
		"custom default leads the new issue")

	// Delete the custom default; a second delete 404s. The copy already on
	// the issue survives.
	delRR := env.do(t, authed(httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/default-questions/%d", custom.ID), nil)))
	require.Equal(t, http.StatusNoContent, delRR.Code)

	againRR := env.do(t, authed(httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/default-questions/%d", custom.ID), nil)))
	assert.Equal(t, http.StatusNotFound, againRR.Code)

	defaults, err = env.store.ListDefaultQuestions(ctx, 1)
	require.NoError(t, err)
	assert.Len(t, defaults, 3, "back to the seeded three")

	questions, err = env.store.ListQuestionsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "What made you laugh out loud?", questions[0].Text,
		"the issue's copy survives the default's deletion")
}

// TestOnboardingQuestionsPerIssue drives the setup wizard end to end and
// checks that the chosen questions-per-issue and default-question switches
// land in settings, and that the first issue carries the enabled defaults
// plus the admin's picks.
func TestOnboardingQuestionsPerIssue(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID := env.createUserWithRole(t, "Admin", "admin@example.com", "admin").ID

	// The wizard runs against a freshly woven second Loop — group 1 is
	// already set up by the test env, and this doubles as multi-group
	// coverage of onboarding.
	groupID, err := env.store.CreateGroup(ctx, "Chaos Crew")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, groupID, adminID, "admin"))

	session := env.sessionCookieForGroup(t, adminID, groupID)
	csrfCookie, csrfHeader := csrfPair()

	defaults, err := env.store.ListDefaultQuestions(ctx, groupID)
	require.NoError(t, err)
	require.Len(t, defaults, 3)
	assert.Equal(t,
		"What good thing happened this month?",
		defaults[0].Text, "migration seeds the reworded prompts")

	// The wizard keeps the first two defaults (swapped into a new order),
	// switches the third off, and promotes one hand-picked prompt to a
	// global default.
	body := fmt.Sprintf(`{
		"admin_name": "Pari",
		"loop_name": "Chaos Crew",
		"frequency": "monthly",
		"submission_window_days": 7,
		"start_datetime": "2026-08-01T09:00",
		"timezone": "Europe/Berlin",
		"questions_per_issue": 8,
		"default_questions": [
			{"id": %d, "enabled": true}, {"id": %d, "enabled": true},
			{"id": %d, "enabled": false}
		],
		"questions": [
			{"text": "A hand-picked prompt?", "category": null, "bank_id": null},
			{"text": "A keeper for every round?", "category": null, "bank_id": null, "make_default": true}
		],
		"invite_emails": []
	}`, defaults[1].ID, defaults[0].ID, defaults[2].ID)

	req := newJSONRequest(http.MethodPost, "/api/onboarding/complete", body)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code, "onboarding: %s", rr.Body.String())

	settings, err := env.store.GetSettings(ctx, groupID)
	require.NoError(t, err)
	assert.Equal(t, 8, settings.QuestionsPerIssue, "wizard choice saved to settings")

	enabled, err := env.store.ListEnabledDefaultQuestions(ctx, groupID)
	require.NoError(t, err)
	require.Len(t, enabled, 3, "wizard switches + promoted pick persist globally")
	assert.Equal(t, defaults[1].Text, enabled[0].Text, "wizard order persisted")
	assert.Equal(t, defaults[0].Text, enabled[1].Text, "wizard order persisted")
	assert.Equal(t, "A keeper for every round?", enabled[2].Text,
		"promoted pick joins the defaults")

	issue, err := env.store.GetActiveIssue(ctx, groupID)
	require.NoError(t, err)

	questions, err := env.store.ListQuestionsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	counts := countBySource(questions)
	assert.Equal(t, 2, counts["default"], "only the kept defaults lead the first issue")
	assert.Equal(t, defaults[1].Text, questions[0].Text,
		"first issue follows the wizard's default order")
	assert.Equal(t, "A keeper for every round?", questions[len(questions)-1].Text,
		"promoted pick keeps its wizard position in the first issue")
}
