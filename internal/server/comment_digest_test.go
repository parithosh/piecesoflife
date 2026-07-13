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

	// Meera replies to Arun's comment on her own answer: Arun (thread
	// participant) hears about the reply, but Meera herself never gets a
	// row for her own words.
	meeraReply := postComment(meera.ID,
		fmt.Sprintf(`{"body":"thank you!","parent_id":%d}`, arunComment))

	pending, err = env.store.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)

	for _, p := range pending {
		switch p.CommentID {
		case arunComment:
			assert.Equal(t, meera.ID, p.RecipientID)
		case meeraReply:
			assert.Equal(t, arun.ID, p.RecipientID,
				"the reply notifies the thread starter, not its author")
		default:
			t.Fatalf("unexpected queued comment %d", p.CommentID)
		}
	}
}

// TestCommentDigestNotifiesThreadParticipants pins who hears about a reply:
// the content owner and everyone who wrote in THAT thread — never the
// replier themselves, and never bystanders whose comments live in a
// different thread on the same piece.
func TestCommentDigestNotifiesThreadParticipants(t *testing.T) {
	env := newIntegrationEnv(t)
	ctx := context.Background()

	meera := env.createUser(t, "Meera", "meera@example.com")
	arun := env.createUser(t, "Arun", "arun@example.com")
	bala := env.createUser(t, "Bala", "bala@example.com")
	divya := env.createUser(t, "Divya", "divya@example.com")

	_, questionIDs := env.seedIssue(t, "published", 7, 2026, 1)

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

	recipientsOf := func(commentID int64) []int64 {
		t.Helper()

		pending, err := env.store.ListPendingCommentNotifications(ctx)
		require.NoError(t, err)

		ids := make([]int64, 0, len(pending))
		for _, p := range pending {
			if p.CommentID == commentID {
				ids = append(ids, p.RecipientID)
			}
		}

		return ids
	}

	// Two separate threads on Meera's answer: Arun starts one, Divya another.
	arunThread := postComment(arun.ID, `{"body":"thread one"}`)
	divyaThread := postComment(divya.ID, `{"body":"thread two"}`)

	assert.ElementsMatch(t, []int64{meera.ID}, recipientsOf(arunThread),
		"top-level comment notifies the owner only")
	assert.ElementsMatch(t, []int64{meera.ID}, recipientsOf(divyaThread))

	// Bala replies in Arun's thread: owner + Arun hear about it — Divya's
	// separate thread does not drag her in, and Bala hears nothing.
	balaReply := postComment(bala.ID,
		fmt.Sprintf(`{"body":"joining thread one","parent_id":%d}`, arunThread))

	assert.ElementsMatch(t, []int64{meera.ID, arun.ID}, recipientsOf(balaReply),
		"reply notifies owner + thread participants, not other threads")

	// Arun replies again in his own thread: Meera (owner) and Bala (earlier
	// participant) hear; Arun himself does not.
	arunReply := postComment(arun.ID,
		fmt.Sprintf(`{"body":"welcome, Bala","parent_id":%d}`, arunThread))

	assert.ElementsMatch(t, []int64{meera.ID, bala.ID}, recipientsOf(arunReply),
		"replier is excluded even in a thread they started")

	// Meera (the owner) replies in the thread: participants hear, she does not.
	meeraReply := postComment(meera.ID,
		fmt.Sprintf(`{"body":"you're both right","parent_id":%d}`, arunThread))

	assert.ElementsMatch(t, []int64{arun.ID, bala.ID}, recipientsOf(meeraReply),
		"owner's own reply notifies participants but never the owner")
}
