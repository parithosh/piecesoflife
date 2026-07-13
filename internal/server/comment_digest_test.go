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

// TestCommentDigestSkipsOwnComments pins the anti-spam rule of the daily
// digest: commenting on your own content — including replying to someone
// else's comment on your own post — must never queue a digest row for you.
// Only other people's comments on your content do.
func TestCommentDigestSkipsOwnComments(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	meera := env.createUser(t, "Meera", "meera@example.com")
	arun := env.createUser(t, "Arun", "arun@example.com")

	issueID, questionIDs := env.seedIssue(t, "published", 7, 2026, 1)
	_ = issueID

	respID, err := env.store.CreateResponse(ctx, meera.ID, questionIDs[0])
	require.NoError(t, err, "create meera's response")

	postComment := func(userID int64, body string) int64 {
		t.Helper()

		csrfCookie, csrfToken := csrfPair()
		req := newJSONRequest(http.MethodPost,
			fmt.Sprintf("/api/responses/%d/comments", respID), body)
		req.AddCookie(env.sessionCookie(t, userID))
		req.AddCookie(csrfCookie)
		req.Header.Set("X-CSRF-Token", csrfToken)

		rr := env.do(t, req)
		require.Equal(t, http.StatusCreated, rr.Code, "post comment: %s", rr.Body)

		var created struct {
			ID int64 `json:"id"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))

		return created.ID
	}

	// Meera comments on her own answer: nothing queues.
	postComment(meera.ID, `{"body":"a note on my own answer"}`)

	pending, err := env.store.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending, "own comment must not queue a digest row")

	// Arun comments on Meera's answer: exactly one row, for Meera.
	arunComment := postComment(arun.ID, `{"body":"lovely answer"}`)

	pending, err = env.store.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, meera.ID, pending[0].RecipientID)

	// Meera replies to Arun's comment on her own answer: still exactly one
	// row — she must not be emailed about her own reply.
	postComment(meera.ID,
		fmt.Sprintf(`{"body":"thank you!","parent_id":%d}`, arunComment))

	pending, err = env.store.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "self-reply on own post must not add a digest row")
	assert.Equal(t, meera.ID, pending[0].RecipientID)
	assert.Equal(t, "lovely answer", pending[0].Body)
}
