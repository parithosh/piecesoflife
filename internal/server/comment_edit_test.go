package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommentEditPermissions pins who may edit a comment: the author and
// nobody else — an admin may delete anyone's comment, but editing another
// person's words stays off-limits even for them.
func TestCommentEditPermissions(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	admin := env.createUserWithRole(t, "Admin", "admin@example.com", "admin")
	meera := env.createUser(t, "Meera", "meera@example.com")
	arun := env.createUser(t, "Arun", "arun@example.com")

	_, questionIDs := env.seedIssue(t, "published", 7, 2026, 1)

	respID, err := env.store.CreateResponse(ctx, meera.ID, questionIDs[0])
	require.NoError(t, err, "create meera's response")

	send := func(userID int64, method, target, body string) *http.Response {
		t.Helper()

		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(method, target, body)
		req.AddCookie(env.sessionCookie(t, userID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)

		return env.do(t, req).Result()
	}

	res := send(arun.ID, http.MethodPost,
		fmt.Sprintf("/api/responses/%d/comments", respID),
		`{"body":"first draft"}`)
	require.Equal(t, http.StatusCreated, res.StatusCode)

	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&created))

	commentTarget := fmt.Sprintf("/api/comments/%d", created.ID)

	// The author edits their own comment: body changes, edited_at stamps.
	res = send(arun.ID, http.MethodPatch, commentTarget, `{"body":"second draft"}`)
	assert.Equal(t, http.StatusOK, res.StatusCode, "author edit must succeed")

	edited, err := env.store.GetCommentByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "second draft", edited.Body)
	assert.NotNil(t, edited.EditedAt, "an edit must stamp edited_at")

	// Another member cannot edit it — not even the piece's owner.
	res = send(meera.ID, http.MethodPatch, commentTarget, `{"body":"hijacked"}`)
	assert.Equal(t, http.StatusForbidden, res.StatusCode,
		"non-author edit must be forbidden")

	// Admins may delete anyone's comment but never rewrite it.
	res = send(admin.ID, http.MethodPatch, commentTarget, `{"body":"admin words"}`)
	assert.Equal(t, http.StatusForbidden, res.StatusCode,
		"admin edit must be forbidden")

	unchanged, err := env.store.GetCommentByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "second draft", unchanged.Body,
		"forbidden edits must not change the body")

	res = send(admin.ID, http.MethodDelete, commentTarget, "")
	assert.Equal(t, http.StatusNoContent, res.StatusCode,
		"admin delete stays allowed")
}
