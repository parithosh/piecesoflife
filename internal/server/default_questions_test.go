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

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	authed := func(req *http.Request) *http.Request {
		req.AddCookie(session)
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfHeader)
		return req
	}

	for i := 0; i < 12; i++ {
		_, err := env.store.CreateBankQuestion(ctx,
			fmt.Sprintf("Bank question %d?", i+1), "fun_silly")
		require.NoError(t, err, "seed bank question")
	}

	// The migration ships the four defaults, all enabled.
	defaults, err := env.store.ListDefaultQuestions(ctx)
	require.NoError(t, err)
	require.Len(t, defaults, 4, "migration seeds four default questions")
	for _, dq := range defaults {
		assert.True(t, dq.Enabled, "defaults start enabled: %s", dq.Text)
	}

	// A fresh issue with the seeded target of 6: the 4 defaults count toward
	// it, so the bank supplies only 2.
	createRR := env.do(t, authed(newJSONRequest(http.MethodPost,
		"/api/issues", `{"title": "Round one"}`)))
	require.Equal(t, http.StatusCreated, createRR.Code, "create: %s", createRR.Body.String())

	first, err := env.store.GetActiveIssue(ctx)
	require.NoError(t, err)

	questions, err := env.store.ListQuestionsByIssue(ctx, first.ID)
	require.NoError(t, err)
	counts := countBySource(questions)
	assert.Equal(t, 4, counts["default"], "all defaults stitched in")
	assert.Equal(t, 2, counts["bank"], "bank pads 6-target minus 4 defaults")
	assert.Len(t, questions, 6, "total hits the target exactly")
	assert.Equal(t, "default", questions[0].Source, "defaults lead a fresh issue")

	// Publishing pre-creates the next draft (auto-create is seeded on).
	pubRR := env.do(t, authed(httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/issues/%d/publish", first.ID), nil)))
	require.Equal(t, http.StatusOK, pubRR.Code, "publish: %s", pubRR.Body.String())

	draft, err := env.store.GetUpcomingDraftIssue(ctx)
	require.NoError(t, err)
	require.NotNil(t, draft, "publish pre-creates the next draft")

	// One friend suggestion lands in the draft. It counts toward the target
	// of 6 alongside the 4 defaults, so the bank supplies exactly 1.
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

	require.NoError(t, env.srv.CreateNextIssue(ctx), "open the draft")

	questions, err = env.store.ListQuestionsByIssue(ctx, draft.ID)
	require.NoError(t, err)
	counts = countBySource(questions)
	assert.Equal(t, "friend", questions[0].Source, "suggestions stay first")
	assert.Equal(t, 1, counts["friend"])
	assert.Equal(t, 4, counts["default"])
	assert.Equal(t, 1, counts["bank"], "4 defaults + 1 friend leave room for 1 bank")
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

	nextDraft, err := env.store.GetUpcomingDraftIssue(ctx)
	require.NoError(t, err)
	require.NotNil(t, nextDraft)
	suggest(nextDraft.ID, 3)

	require.NoError(t, env.srv.CreateNextIssue(ctx), "open round three")

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

// TestOnboardingQuestionsPerIssue drives the setup wizard end to end and
// checks that the chosen questions-per-issue and default-question switches
// land in settings, and that the first issue carries the enabled defaults
// plus the admin's picks.
func TestOnboardingQuestionsPerIssue(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	adminID, err := env.store.CreateUser(ctx, "Admin", "admin@example.com", "admin")
	require.NoError(t, err, "create admin")
	session := env.sessionCookie(t, adminID)
	csrfCookie, csrfHeader := csrfPair()

	defaults, err := env.store.ListDefaultQuestions(ctx)
	require.NoError(t, err)
	require.Len(t, defaults, 4)
	assert.Equal(t,
		"Tell us about one good thing that happened to you this month!",
		defaults[0].Text, "migration seeds the full wording")

	// The wizard keeps the first two defaults and switches the rest off.
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
			{"id": %d, "enabled": false}, {"id": %d, "enabled": false}
		],
		"questions": [{"text": "A hand-picked prompt?", "category": null, "bank_id": null}],
		"invite_emails": []
	}`, defaults[0].ID, defaults[1].ID, defaults[2].ID, defaults[3].ID)

	req := newJSONRequest(http.MethodPost, "/api/onboarding/complete", body)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)
	require.Equal(t, http.StatusOK, rr.Code, "onboarding: %s", rr.Body.String())

	settings, err := env.store.GetSettings(ctx)
	require.NoError(t, err)
	assert.Equal(t, 8, settings.QuestionsPerIssue, "wizard choice saved to settings")

	enabled, err := env.store.ListEnabledDefaultQuestions(ctx)
	require.NoError(t, err)
	require.Len(t, enabled, 2, "wizard switches persist globally")
	assert.Equal(t, defaults[0].Text, enabled[0].Text)
	assert.Equal(t, defaults[1].Text, enabled[1].Text)

	issue, err := env.store.GetActiveIssue(ctx)
	require.NoError(t, err)

	questions, err := env.store.ListQuestionsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	counts := countBySource(questions)
	assert.Equal(t, 2, counts["default"], "only the kept defaults lead the first issue")
	assert.Equal(t, "A hand-picked prompt?", questions[len(questions)-1].Text,
		"admin's pick follows the defaults")
}
