package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// weaveSecondLoop creates a second, fully set-up Loop and returns its ID.
func weaveSecondLoop(t *testing.T, env *integrationEnv, name string) int64 {
	t.Helper()
	ctx := context.Background()

	groupID, err := env.store.CreateGroup(ctx, name, nil)
	require.NoError(t, err, "create second group")
	require.NoError(t, env.store.CompleteSetup(ctx, groupID), "complete second group setup")

	return groupID
}

// seedIssueInGroup mirrors integrationEnv.seedIssue for an arbitrary Loop.
func seedIssueInGroup(
	t *testing.T, env *integrationEnv, groupID int64, status string,
) (int64, int64) {
	t.Helper()
	ctx := context.Background()

	now := time.Now().UTC()

	issueID, err := env.store.CreateIssue(ctx, groupID, nil, 7, 2026,
		now, now.Add(7*24*time.Hour))
	require.NoError(t, err, "create issue")
	require.NoError(t, env.store.SetIssueStatus(ctx, issueID, status), "set status")

	questionID, err := env.store.CreateQuestion(ctx, issueID,
		"Isolation question?", nil, "bank", nil, 0)
	require.NoError(t, err, "create question")

	return issueID, questionID
}

// TestGroupDataIsolation verifies that a member of one Loop cannot read or
// touch another Loop's issues through the API.
func TestGroupDataIsolation(t *testing.T) {
	env := newIntegrationEnv(t)

	groupB := weaveSecondLoop(t, env, "Loop B")

	// alice belongs only to group 1; the issue lives in group B.
	alice := env.createUser(t, "Alice", "alice-iso@example.com")
	issueB, _ := seedIssueInGroup(t, env, groupB, "collecting")

	session := env.sessionCookie(t, alice.ID)

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/issues/%d", issueB), nil)
	req.AddCookie(session)
	rr := env.do(t, req)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"another Loop's issue must read as not found")

	// The issue list only shows the current Loop's issues.
	listReq := httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	listReq.AddCookie(session)
	listRR := env.do(t, listReq)

	require.Equal(t, http.StatusOK, listRR.Code)
	assert.NotContains(t, listRR.Body.String(), "Isolation question?")
	assert.NotContains(t, listRR.Body.String(),
		fmt.Sprintf(`"id":%d`, issueB), "Loop B's issue must not be listed")
}

// TestGroupAutoSwitchOnGet verifies the resource-based Loop switch: a person
// who belongs to both Loops opens a link into the other Loop and is
// transparently switched instead of 404ing.
func TestGroupAutoSwitchOnGet(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	dual := env.createUser(t, "Dua", "dua@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, dual.ID, "member"))

	issueB, _ := seedIssueInGroup(t, env, groupB, "collecting")

	// Current Loop is 1; a PAGE navigation into Loop B redirects (the
	// switch) …
	session := env.sessionCookie(t, dual.ID)

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", issueB), nil)
	req.AddCookie(session)
	rr := env.do(t, req)

	require.Equal(t, http.StatusSeeOther, rr.Code,
		"cross-Loop page GET should switch and replay")

	// … and the replayed request (same session, now switched) succeeds.
	again := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond", issueB), nil)
	again.AddCookie(session)
	againRR := env.do(t, again)

	assert.Equal(t, http.StatusOK, againRR.Code,
		"after the switch the issue is readable")

	// API GETs never switch: reset to Loop 1, then fetch a Loop-B issue —
	// a stale tab's fetch must not flip the session's Loop underneath the
	// user, so it reads as not found instead.
	require.NoError(t, env.store.SetLastGroup(context.Background(), dual.ID, 1))

	apiSession := env.sessionCookie(t, dual.ID)

	apiReq := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/issues/%d", issueB), nil)
	apiReq.AddCookie(apiSession)
	assert.Equal(t, http.StatusNotFound, env.do(t, apiReq).Code,
		"cross-Loop API GET must 404, not switch")
}

// TestGroupRoleIsolation verifies that being an admin in one Loop grants
// nothing in another.
func TestGroupRoleIsolation(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	// keeper administers group 1 but is a plain member of group B.
	keeper := env.createUserWithRole(t, "Keeper", "keeper@example.com", "admin")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, keeper.ID, "member"))

	// With group B current, admin APIs must refuse.
	session := env.sessionCookieForGroup(t, keeper.ID, groupB)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	req.AddCookie(session)
	rr := env.do(t, req)

	assert.Equal(t, http.StatusForbidden, rr.Code,
		"Loop-A admin must not administer Loop B")

	// Back in their own Loop the same call succeeds.
	homeSession := env.sessionCookie(t, keeper.ID)

	homeReq := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	homeReq.AddCookie(homeSession)
	homeRR := env.do(t, homeReq)

	assert.Equal(t, http.StatusOK, homeRR.Code)
}

// TestInviteExistingAccountJoinsLoop verifies that inviting an email address
// that already has an account simply adds a membership — one identity, many
// Loops.
func TestInviteExistingAccountJoinsLoop(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	// bob already exists as a member of group B.
	bob, err := env.store.CreateUser(ctx, "Bob", "bob@example.com")
	require.NoError(t, err)
	require.NoError(t, env.store.CreateMembership(ctx, groupB, bob, "member"))

	admin := env.createUserWithRole(t, "Admin", "admin-inv@example.com", "admin")
	session := env.sessionCookie(t, admin.ID)
	csrfCookie, csrfHeader := csrfPair()

	req := newJSONRequest(http.MethodPost, "/api/users/invite",
		`{"email": "bob@example.com"}`)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)

	require.Equal(t, http.StatusOK, rr.Code, "invite existing account: %s", rr.Body.String())

	m, err := env.store.GetMembership(ctx, 1, bob)
	require.NoError(t, err, "bob gains a membership in group 1")
	assert.True(t, m.IsActive)
	assert.Equal(t, "member", m.Role)

	// Still one identity.
	_, err = env.store.GetUserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)

	groups, err := env.store.ListUserGroups(ctx, bob)
	require.NoError(t, err)
	assert.Len(t, groups, 2, "bob now belongs to both Loops")
}

// TestInstanceConsoleAccess verifies the operator console is gated to
// instance admins, and that creating a Loop through it bootstraps the
// keeper's account and membership.
func TestInstanceConsoleAccess(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	// A regular Loop admin is NOT an instance admin.
	admin := env.createUserWithRole(t, "Admin", "admin-console@example.com", "admin")
	session := env.sessionCookie(t, admin.ID)

	apiReq := httptest.NewRequest(http.MethodGet, "/api/instance/groups", nil)
	apiReq.AddCookie(session)
	assert.Equal(t, http.StatusForbidden, env.do(t, apiReq).Code,
		"Loop admins have no instance API access")

	// The seeded operator is.
	require.NoError(t, env.store.SeedAdminUser(ctx, "operator@example.com"))
	operator, err := env.store.GetUserByEmail(ctx, "operator@example.com")
	require.NoError(t, err)
	require.True(t, operator.IsInstanceAdmin)

	opSession := env.sessionCookie(t, operator.ID)
	csrfCookie, csrfHeader := csrfPair()

	createReq := newJSONRequest(http.MethodPost, "/api/instance/groups",
		`{"name": "The Sharma Family", "admin_email": "asha@example.com", "admin_name": "Asha"}`)
	createReq.AddCookie(opSession)
	createReq.AddCookie(csrfCookie)
	createReq.Header.Set("X-CSRF-Token", csrfHeader)
	createRR := env.do(t, createReq)

	require.Equal(t, http.StatusCreated, createRR.Code,
		"create loop: %s", createRR.Body.String())

	// The keeper account and admin membership exist; the Loop starts with
	// its own settings and default questions, still awaiting setup.
	asha, err := env.store.GetUserByEmail(ctx, "asha@example.com")
	require.NoError(t, err, "keeper account created")

	groups, err := env.store.ListUserGroups(ctx, asha.ID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "The Sharma Family", groups[0].LoopName)
	assert.Equal(t, "admin", groups[0].Role)
	assert.False(t, groups[0].SetupComplete)

	defaults, err := env.store.ListDefaultQuestions(ctx, groups[0].GroupID)
	require.NoError(t, err)
	assert.Len(t, defaults, 3, "new Loop starts with the standard defaults")
}

// TestSwitchGroupEndpoint verifies the explicit switcher endpoint validates
// membership.
func TestSwitchGroupEndpoint(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	solo := env.createUser(t, "Solo", "solo@example.com")
	dual := env.createUser(t, "Dual", "dual@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, dual.ID, "member"))

	csrfCookie, csrfHeader := csrfPair()

	// solo cannot switch into a Loop they don't belong to.
	soloReq := newJSONRequest(http.MethodPost, "/api/me/group",
		fmt.Sprintf(`{"group_id": %d}`, groupB))
	soloReq.AddCookie(env.sessionCookie(t, solo.ID))
	soloReq.AddCookie(csrfCookie)
	soloReq.Header.Set("X-CSRF-Token", csrfHeader)
	assert.Equal(t, http.StatusNotFound, env.do(t, soloReq).Code)

	// dual can, and their next request runs in the new Loop.
	dualSession := env.sessionCookie(t, dual.ID)

	dualReq := newJSONRequest(http.MethodPost, "/api/me/group",
		fmt.Sprintf(`{"group_id": %d}`, groupB))
	dualReq.AddCookie(dualSession)
	dualReq.AddCookie(csrfCookie)
	dualReq.Header.Set("X-CSRF-Token", csrfHeader)
	require.Equal(t, http.StatusOK, env.do(t, dualReq).Code)

	user, err := env.store.GetUserByID(ctx, dual.ID)
	require.NoError(t, err)
	require.NotNil(t, user.LastGroupID)
	assert.Equal(t, groupB, *user.LastGroupID, "switch persists as last Loop")
}

// TestCrossLoopCommentDeleteBlocked verifies a Loop admin cannot delete
// comments living in another Loop by walking IDs.
func TestCrossLoopCommentDeleteBlocked(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	// A comment on a response in Loop B.
	author := env.createUser(t, "Author", "author-b@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, author.ID, "member"))

	_, questionB := seedIssueInGroup(t, env, groupB, "collecting")

	respID, err := env.store.CreateResponse(ctx, author.ID, questionB)
	require.NoError(t, err)

	commentID, err := env.store.CreateComment(ctx, author.ID, respID, nil, "loop B secret")
	require.NoError(t, err)

	// The admin of Loop A tries to delete it.
	adminA := env.createUserWithRole(t, "AdminA", "admin-a@example.com", "admin")
	session := env.sessionCookie(t, adminA.ID)
	csrfCookie, csrfHeader := csrfPair()

	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/comments/%d", commentID), nil)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"cross-Loop comment delete must read as not found")

	comments, err := env.store.ListCommentsByResponse(ctx, respID)
	require.NoError(t, err)
	assert.Len(t, comments, 1, "the comment survives")
}

// TestReinviteDoesNotDemoteAdmin verifies CreateMembership's upsert never
// downgrades an existing admin membership (a keeper listing themselves in
// their own wizard's invite list must not lose the Loop).
func TestReinviteDoesNotDemoteAdmin(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	keeper := env.createUserWithRole(t, "Keeper", "keeper-demote@example.com", "admin")

	require.NoError(t, env.store.CreateMembership(ctx, 1, keeper.ID, "member"))

	m, err := env.store.GetMembership(ctx, 1, keeper.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", m.Role, "re-invite as member must not demote an admin")

	// Promotion through the same path still works.
	member := env.createUser(t, "Riser", "riser@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, 1, member.ID, "admin"))

	m, err = env.store.GetMembership(ctx, 1, member.ID)
	require.NoError(t, err)
	assert.Equal(t, "admin", m.Role)
}

// TestAutoSwitchStripsGroupOverride verifies a stale ?g= that contradicts
// the issue's Loop cannot cause a redirect loop: the switch redirect drops
// the parameter.
func TestAutoSwitchStripsGroupOverride(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	dual := env.createUser(t, "Dual", "dual-g@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, dual.ID, "member"))

	issueB, _ := seedIssueInGroup(t, env, groupB, "collecting")

	session := env.sessionCookie(t, dual.ID)

	// ?g=1 contradicts the issue's Loop (B): the switch must strip it.
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/issues/%d/respond?g=1", issueB), nil)
	req.AddCookie(session)
	rr := env.do(t, req)

	require.Equal(t, http.StatusSeeOther, rr.Code)
	assert.NotContains(t, rr.Header().Get("Location"), "g=1",
		"replayed URL must not carry the stale override")

	// The replayed request terminates with the issue, not another redirect.
	again := httptest.NewRequest(http.MethodGet, rr.Header().Get("Location"), nil)
	again.AddCookie(session)
	assert.Equal(t, http.StatusOK, env.do(t, again).Code)
}

// TestCrossLoopDumpDeleteBlocked verifies dump items can't be deleted
// across Loops — the guard fails closed and probing reads as not found.
func TestCrossLoopDumpDeleteBlocked(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")

	owner := env.createUser(t, "Owner", "owner-dump@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupB, owner.ID, "member"))

	issueB, _ := seedIssueInGroup(t, env, groupB, "collecting")

	itemID, err := env.store.CreateDumpItem(ctx, issueB, owner.ID, "photo",
		nil, "/tmp/nonexistent-dump.jpg", nil)
	require.NoError(t, err)

	adminA := env.createUserWithRole(t, "AdminA", "admin-dump@example.com", "admin")
	session := env.sessionCookie(t, adminA.ID)
	csrfCookie, csrfHeader := csrfPair()

	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/dump/%d", itemID), nil)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)

	assert.Equal(t, http.StatusNotFound, rr.Code,
		"cross-Loop dump delete must read as not found")

	_, err = env.store.GetDumpItemByID(ctx, itemID)
	assert.NoError(t, err, "the item survives")
}

// TestArchiveCancelsPendingEvents verifies archiving a Loop deletes its
// pending scheduler events so it stops closing rounds and emailing.
func TestArchiveCancelsPendingEvents(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupB := weaveSecondLoop(t, env, "Loop B")
	issueB, _ := seedIssueInGroup(t, env, groupB, "collecting")

	require.NoError(t, env.store.CreateSchedulerEvent(ctx, &issueB,
		"auto_close", time.Now().Add(48*time.Hour)))

	pending, err := env.store.GetNextPendingEventForGroup(ctx, "auto_close", groupB)
	require.NoError(t, err)
	require.NotNil(t, pending)

	require.NoError(t, env.store.SeedAdminUser(ctx, "operator-arch@example.com"))
	operator, err := env.store.GetUserByEmail(ctx, "operator-arch@example.com")
	require.NoError(t, err)

	session := env.sessionCookie(t, operator.ID)
	csrfCookie, csrfHeader := csrfPair()

	req := newJSONRequest(http.MethodPatch,
		fmt.Sprintf("/api/instance/groups/%d", groupB), `{"is_active": false}`)
	req.AddCookie(session)
	req.AddCookie(csrfCookie)
	req.Header.Set("X-CSRF-Token", csrfHeader)
	rr := env.do(t, req)

	require.Equal(t, http.StatusOK, rr.Code, "archive: %s", rr.Body.String())

	pending, err = env.store.GetNextPendingEventForGroup(ctx, "auto_close", groupB)
	require.NoError(t, err)
	assert.Nil(t, pending, "archiving cancels the Loop's pending events")
}

// TestReconcileAutoCreateRequeues verifies the reconcile sweep re-queues a
// stalled auto-create cycle: auto-create on, nothing collecting, and no
// pending next-round event.
func TestReconcileAutoCreateRequeues(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	// Group 1 has a published round and nothing queued — a stalled cycle.
	issueID, _ := env.seedIssue(t, "published", 6, 2026, 1)
	require.NoError(t, env.store.PublishIssue(ctx, issueID))

	pending, err := env.store.GetNextPendingEventForGroup(ctx, "create_next_issue", 1)
	require.NoError(t, err)
	require.Nil(t, pending, "precondition: nothing queued")

	require.NoError(t, env.srv.ReconcileAutoCreate(ctx))

	pending, err = env.store.GetNextPendingEventForGroup(ctx, "create_next_issue", 1)
	require.NoError(t, err)
	require.NotNil(t, pending, "reconcile queues the next-round event")
	require.NotNil(t, pending.IssueID, "the event references the pre-created draft")

	draft, err := env.store.GetNextDraftIssue(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, draft, "reconcile pre-created the draft")
	assert.Equal(t, *pending.IssueID, draft.ID)
}

// TestSetupGateForMembers verifies members of a not-yet-set-up Loop are held
// at the Loop list, while its admin reaches the wizard.
func TestSetupGateForMembers(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	groupID, err := env.store.CreateGroup(ctx, "Unwoven Loop", nil)
	require.NoError(t, err)

	member := env.createUser(t, "Mem", "mem@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupID, member.ID, "member"))

	session := env.sessionCookieForGroup(t, member.ID, groupID)

	req := httptest.NewRequest(http.MethodGet, "/issues", nil)
	req.AddCookie(session)
	rr := env.do(t, req)

	require.Equal(t, http.StatusSeeOther, rr.Code)
	assert.Equal(t, "/loops", rr.Header().Get("Location"),
		"members wait at the Loop list until setup completes")

	keeper := env.createUser(t, "Keep", "keep-gate@example.com")
	require.NoError(t, env.store.CreateMembership(ctx, groupID, keeper.ID, "admin"))

	keeperSession := env.sessionCookieForGroup(t, keeper.ID, groupID)

	keeperReq := httptest.NewRequest(http.MethodGet, "/admin/setup", nil)
	keeperReq.AddCookie(keeperSession)
	keeperRR := env.do(t, keeperReq)

	assert.Equal(t, http.StatusOK, keeperRR.Code,
		"the keeper reaches the wizard for their Loop")
}
