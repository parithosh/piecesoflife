package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubmissionProgressExcludesAdmins locks in the "Pallu" product decision:
// admins are left out of the progress denominator by default, and folded back
// in only when the issue's count_admin_in flag is set. The roster always lists
// everyone, with admins sorted last.
func TestSubmissionProgressExcludesAdmins(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	issueID, err := st.CreateIssue(ctx, nil, int(now.Month()), now.Year(),
		now, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, st.SetIssueStatus(ctx, issueID, "collecting"))

	questionID, err := st.CreateQuestion(ctx, issueID, "What should we remember?",
		nil, "bank", nil, 0)
	require.NoError(t, err)

	memberID := seedUser(t, st, "friend", "friend@example.com")

	adminID, err := st.CreateUser(ctx, "Pari", "admin@example.com", "admin")
	require.NoError(t, err)

	// Only the admin submits an answer; the member hasn't responded.
	adminResp, err := st.CreateResponse(ctx, adminID, questionID)
	require.NoError(t, err)
	require.NoError(t, st.SubmitResponse(ctx, adminResp))

	// Default: admin excluded from the counts, still shown in the roster last.
	prog, err := st.GetSubmissionProgress(ctx, issueID)
	require.NoError(t, err)
	assert.False(t, prog.CountAdminIn)
	assert.Equal(t, 1, prog.TotalMembers, "denominator excludes the admin")
	assert.Equal(t, 0, prog.Responded, "admin's submission is not counted")
	require.Len(t, prog.Members, 2, "roster still lists everyone")
	assert.Equal(t, memberID, prog.Members[0].User.ID, "non-admins sort first")
	assert.Equal(t, adminID, prog.Members[1].User.ID, "admins sort last")

	// Opt the admin in: denominator and responded count grow by one.
	require.NoError(t, st.SetIssueCountAdminIn(ctx, issueID, true))

	prog, err = st.GetSubmissionProgress(ctx, issueID)
	require.NoError(t, err)
	assert.True(t, prog.CountAdminIn)
	assert.Equal(t, 2, prog.TotalMembers, "admin folded into denominator")
	assert.Equal(t, 1, prog.Responded, "admin's submission now counts")
}
