package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDumpItemLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	userA := seedUser(t, s, "Zara", "zara@example.com")
	userB := seedUser(t, s, "Anil", "anil@example.com")

	now := time.Now().UTC()
	issueID, err := s.CreateIssue(ctx, nil, 7, 2026, now, now.Add(7*24*time.Hour))
	require.NoError(t, err)

	caption := "beach day"
	ct := "video/webm"

	id1, err := s.CreateDumpItem(ctx, issueID, userA, "photo", nil, "/2026/07/a1.jpg", &caption)
	require.NoError(t, err)
	_, err = s.CreateDumpItem(ctx, issueID, userA, "photo", nil, "/2026/07/a2.jpg", nil)
	require.NoError(t, err)
	_, err = s.CreateDumpItem(ctx, issueID, userA, "video", &ct, "/2026/07/a3.webm", nil)
	require.NoError(t, err)
	_, err = s.CreateDumpItem(ctx, issueID, userB, "photo", nil, "/2026/07/b1.jpg", nil)
	require.NoError(t, err)

	// sort_order counts up per (issue, user).
	item, err := s.GetDumpItemByID(ctx, id1)
	require.NoError(t, err)
	assert.Equal(t, 0, item.SortOrder)
	assert.Equal(t, "beach day", *item.Caption)

	// Per-user, per-kind caps see the right counts.
	photos, err := s.CountDumpItemsForUser(ctx, issueID, userA, "photo")
	require.NoError(t, err)
	assert.Equal(t, 2, photos)

	videos, err := s.CountDumpItemsForUser(ctx, issueID, userA, "video")
	require.NoError(t, err)
	assert.Equal(t, 1, videos)

	// Issue-wide listing groups by user name (Anil before Zara), each in
	// upload order, with profile fields joined in.
	all, err := s.ListDumpItemsByIssue(ctx, issueID)
	require.NoError(t, err)
	require.Len(t, all, 4)
	assert.Equal(t, "Anil", all[0].UserName)
	assert.Equal(t, "Zara", all[1].UserName)
	assert.Equal(t, 0, all[1].SortOrder)
	assert.Equal(t, 2, all[3].SortOrder)

	// Own-items listing is scoped to one member.
	mine, err := s.ListDumpItemsForUser(ctx, issueID, userA)
	require.NoError(t, err)
	require.Len(t, mine, 3)

	// Delete removes exactly one row and reports missing IDs.
	deleted, err := s.DeleteDumpItem(ctx, id1)
	require.NoError(t, err)
	assert.True(t, deleted)

	deleted, err = s.DeleteDumpItem(ctx, id1)
	require.NoError(t, err)
	assert.False(t, deleted)

	mine, err = s.ListDumpItemsForUser(ctx, issueID, userA)
	require.NoError(t, err)
	assert.Len(t, mine, 2)
}
