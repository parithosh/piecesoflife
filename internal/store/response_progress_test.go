package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubmissionProgressCountsAdminsByDefault locks in the "Pallu" product
// decision: admins count toward the progress denominator by default, and are
// left out only when the issue's count_admin_in flag is switched off. The
// roster always lists everyone, with admins sorted last.
func TestSubmissionProgressCountsAdminsByDefault(t *testing.T) {
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

	memberID := seedUser(t, st, "friend", "friend@example.com")

	adminID, err := st.CreateUser(ctx, "Pari", "admin@example.com")
	require.NoError(t, err)
	err = st.CreateMembership(ctx, 1, adminID, "admin")
	require.NoError(t, err)

	// Only the admin submits an answer; the member hasn't responded.
	adminResp, err := st.CreateResponse(ctx, adminID, questionID)
	require.NoError(t, err)
	require.NoError(t, st.SubmitResponse(ctx, adminResp))

	// Default: admin included in the counts, shown in the roster last.
	prog, err := st.GetSubmissionProgress(ctx, issueID)
	require.NoError(t, err)
	assert.True(t, prog.CountAdminIn)
	assert.Equal(t, 2, prog.TotalMembers, "denominator includes the admin")
	assert.Equal(t, 1, prog.Responded, "admin's submission is counted")
	require.Len(t, prog.Members, 2, "roster lists everyone")
	assert.Equal(t, memberID, prog.Members[0].User.ID, "non-admins sort first")
	assert.Equal(t, adminID, prog.Members[1].User.ID, "admins sort last")

	// Opt the admin out: denominator and responded count shrink by one.
	require.NoError(t, st.SetIssueCountAdminIn(ctx, issueID, false))

	prog, err = st.GetSubmissionProgress(ctx, issueID)
	require.NoError(t, err)
	assert.False(t, prog.CountAdminIn)
	assert.Equal(t, 1, prog.TotalMembers, "admin left out of denominator")
	assert.Equal(t, 0, prog.Responded, "admin's submission no longer counts")
}
