package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetIssueByResponseID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, 1, nil, int(now.Month()), now.Year(),
		now, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, st.SetIssueStatus(ctx, issueID, "collecting"))

	questionID, err := st.CreateQuestion(ctx, issueID, "What should we remember?",
		nil, "bank", nil, 0)
	require.NoError(t, err)

	userID := seedUser(t, st, "friend", "friend@example.com")
	responseID, err := st.CreateResponse(ctx, userID, questionID)
	require.NoError(t, err)

	byResponse, err := st.GetIssueByResponseID(ctx, responseID)
	require.NoError(t, err)
	assert.Equal(t, issueID, byResponse.ID)
	assert.Equal(t, "collecting", byResponse.Status)
}
