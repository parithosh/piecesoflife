package store

import (
	"context"
	"database/sql"
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

func TestOpenDraftEarlyRollsBackQueuedOpening(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	oldOpen := now.AddDate(0, 1, 0)
	oldDeadline := oldOpen.AddDate(0, 0, 7)

	draftID, err := st.CreateIssue(ctx, 1, nil,
		int(oldOpen.Month()), oldOpen.Year(), oldOpen, oldDeadline)
	require.NoError(t, err)
	require.NoError(t, st.CreateSchedulerEvent(ctx, &draftID,
		"create_next_issue", oldOpen))

	newOpen := now
	newDeadline := now.AddDate(0, 0, 7)
	err = st.OpenDraftEarly(
		ctx, 1, draftID, false, nil,
		int(newOpen.Month()), newOpen.Year(), newOpen, newDeadline,
		[]SchedulerEventSpec{{EventType: "not_an_event", ScheduledAt: newDeadline}},
	)
	require.Error(t, err)

	draft, err := st.GetIssueByID(ctx, draftID)
	require.NoError(t, err)
	assert.Equal(t, "draft", draft.Status)
	assert.True(t, draft.OpensAt.Equal(oldOpen))
	assert.True(t, draft.Deadline.Equal(oldDeadline))

	pending, err := st.GetPendingNextRoundEvent(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, pending, "failed early open restores the queued opening")
	assert.Equal(t, draftID, *pending.IssueID)
	assert.True(t, pending.ScheduledAt.Equal(oldOpen))
}

func TestUpdateDraftIssueRejectsAnOpenedRound(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	oldDeadline := now.AddDate(0, 0, 7)

	issueID, err := st.CreateIssue(ctx, 1, nil,
		int(now.Month()), now.Year(), now, oldDeadline)
	require.NoError(t, err)
	require.NoError(t, st.SetIssueStatus(ctx, issueID, "collecting"))

	newDeadline := oldDeadline.AddDate(0, 0, 3)
	err = st.UpdateDraftIssue(ctx, issueID, nil, newDeadline)
	require.ErrorIs(t, err, sql.ErrNoRows)

	issue, err := st.GetIssueByID(ctx, issueID)
	require.NoError(t, err)
	assert.Equal(t, "collecting", issue.Status)
	assert.True(t, issue.Deadline.Equal(oldDeadline),
		"a concurrent open must leave the collecting deadline untouched")
}
