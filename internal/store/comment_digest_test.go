package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDumpCommentsAndEditing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	meera := seedUser(t, s, "Meera", "meera@example.com")
	arun := seedUser(t, s, "Arun", "arun@example.com")

	now := time.Now().UTC()
	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	itemID, err := s.CreateDumpItem(ctx, issueID, meera, "photo", nil,
		"/up/2026/07/dump.jpg", nil)
	require.NoError(t, err)

	// Comment on a dump item, then edit it.
	commentID, err := s.CreateDumpComment(ctx, arun, itemID, nil, "the good light!")
	require.NoError(t, err)

	comments, err := s.ListCommentsByDumpItem(ctx, itemID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.NotNil(t, comments[0].DumpItemID)
	assert.Nil(t, comments[0].EditedAt)

	require.NoError(t, s.UpdateCommentBody(ctx, commentID, "THE good light!"))

	updated, err := s.GetCommentByID(ctx, commentID)
	require.NoError(t, err)
	assert.Equal(t, "THE good light!", updated.Body)
	assert.NotNil(t, updated.EditedAt)

	// The dump item resolves to its issue for the Loop guard.
	issue, err := s.GetIssueByDumpItemID(ctx, itemID)
	require.NoError(t, err)
	assert.Equal(t, issueID, issue.ID)
}

func TestCommentNotificationQueue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	meera := seedUser(t, s, "Meera", "meera@example.com")
	arun := seedUser(t, s, "Arun", "arun@example.com")

	now := time.Now().UTC()
	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	itemID, err := s.CreateDumpItem(ctx, issueID, meera, "photo", nil,
		"/up/2026/07/dump.jpg", nil)
	require.NoError(t, err)

	c1, err := s.CreateDumpComment(ctx, arun, itemID, nil, "first!")
	require.NoError(t, err)
	c2, err := s.CreateDumpComment(ctx, arun, itemID, nil, "second!")
	require.NoError(t, err)

	require.NoError(t, s.EnqueueCommentNotification(ctx, meera, c1))
	require.NoError(t, s.EnqueueCommentNotification(ctx, meera, c2))
	// Duplicate enqueues are ignored.
	require.NoError(t, s.EnqueueCommentNotification(ctx, meera, c1))

	pending, err := s.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, meera, pending[0].RecipientID)
	assert.Equal(t, "Arun", pending[0].CommenterName)
	assert.Equal(t, "meera@example.com", pending[0].RecipientEmail)
	require.NotNil(t, pending[0].DumpItemID)

	// Deleting the comment takes its queued notification with it (CASCADE);
	// the digest reads current bodies, so an edit shows up too.
	require.NoError(t, s.UpdateCommentBody(ctx, c2, "second, edited"))
	require.NoError(t, s.DeleteComment(ctx, c1))

	pending, err = s.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "second, edited", pending[0].Body)

	// Draining removes exactly the given rows.
	require.NoError(t, s.DeleteCommentNotifications(ctx,
		[]int64{pending[0].NotificationID}))

	pending, err = s.ListPendingCommentNotifications(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending)
}
