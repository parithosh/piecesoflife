package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func mustParseTime(t *testing.T, day string) time.Time {
	t.Helper()

	parsed, err := time.Parse("2006-01-02", day)
	require.NoError(t, err)

	return parsed
}

func TestRambleAutosaveLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Meera", "meera@example.com")

	// First save creates the day.
	id, removed, err := s.AutosaveRambleDay(ctx, user, "2026-06-23",
		[]RambleBlock{{Type: "text", Content: strPtr("The gulmohar flowered.")}})
	require.NoError(t, err)
	assert.False(t, removed)
	assert.NotZero(t, id)

	// Re-saving replaces the text without duplicating blocks.
	id2, removed, err := s.AutosaveRambleDay(ctx, user, "2026-06-23",
		[]RambleBlock{{Type: "text", Content: strPtr("The gulmohar finally flowered.")}})
	require.NoError(t, err)
	assert.False(t, removed)
	assert.Equal(t, id, id2)

	blocks, err := s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, "The gulmohar finally flowered.", *blocks[0].Content)

	// Media blocks survive text autosaves.
	_, err = s.CreateRambleBlock(ctx, id, "photo", nil, strPtr("/up/2026/06/x.jpg"), nil)
	require.NoError(t, err)

	_, removed, err = s.AutosaveRambleDay(ctx, user, "2026-06-23",
		[]RambleBlock{{Type: "text", Content: strPtr("Edited again.")}})
	require.NoError(t, err)
	assert.False(t, removed)

	blocks, err = s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	assert.Len(t, blocks, 2)

	// Clearing the text with media still present keeps the day.
	_, removed, err = s.AutosaveRambleDay(ctx, user, "2026-06-23", nil)
	require.NoError(t, err)
	assert.False(t, removed)

	// Deleting the last media block deletes the now-empty day with it.
	blocks, err = s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.NoError(t, s.DeleteRambleBlock(ctx, blocks[0].ID))

	_, err = s.GetRambleByDay(ctx, user, "2026-06-23")
	assert.Error(t, err, "empty day should be invisible")

	// An empty save on a day that never existed stays invisible.
	_, removed, err = s.AutosaveRambleDay(ctx, user, "2026-06-24", nil)
	require.NoError(t, err)
	assert.True(t, removed)

	days, err := s.ListRambleDays(ctx, user)
	require.NoError(t, err)
	assert.Empty(t, days)
}

func TestRambleDayListingAndCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Arun", "arun@example.com")
	other := seedUser(t, s, "Zara", "zara@example.com")

	for _, day := range []string{"2026-06-19", "2026-06-29", "2026-07-08"} {
		_, _, err := s.AutosaveRambleDay(ctx, user, day,
			[]RambleBlock{{Type: "text", Content: strPtr("note on " + day)}})
		require.NoError(t, err)
	}

	_, _, err := s.AutosaveRambleDay(ctx, other, "2026-07-01",
		[]RambleBlock{{Type: "text", Content: strPtr("someone else's note")}})
	require.NoError(t, err)

	// Newest first, scoped to the owner, blocks attached.
	days, err := s.ListRambleDays(ctx, user)
	require.NoError(t, err)
	require.Len(t, days, 3)
	assert.Equal(t, "2026-07-08", days[0].Ramble.Day)
	assert.Equal(t, "2026-06-19", days[2].Ramble.Day)
	require.Len(t, days[0].Blocks, 1)

	// Window counts are inclusive on both ends.
	n, err := s.CountRambleDaysBetween(ctx, user, "2026-06-29", "2026-07-08")
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	n, err = s.CountRambleDaysBetween(ctx, user, "", "2026-07-08")
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestDiaryAttachSnapshotsJournal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Meera", "meera@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-06-23",
		[]RambleBlock{{Type: "text", Content: strPtr("gulmohar")}})
	require.NoError(t, err)

	rambleID, _, err := s.AutosaveRambleDay(ctx, user, "2026-06-27",
		[]RambleBlock{{Type: "text", Content: strPtr("evening walk")}})
	require.NoError(t, err)
	_, err = s.CreateRambleBlock(ctx, rambleID, "photo", nil,
		strPtr("/up/2026/06/sky.jpg"), strPtr("the ninety-second sky"))
	require.NoError(t, err)

	// A day outside the window stays out of the snapshot.
	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-05-01",
		[]RambleBlock{{Type: "text", Content: strPtr("too old")}})
	require.NoError(t, err)

	sectionID, copied, err := s.AttachDiarySection(ctx, issueID, user,
		"2026-06-01", "2026-07-05")
	require.NoError(t, err)
	assert.Equal(t, 2, copied)

	// Double-attach conflicts.
	_, _, err = s.AttachDiarySection(ctx, issueID, user, "2026-06-01", "2026-07-05")
	assert.ErrorIs(t, err, ErrDiaryAttached)

	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	require.Len(t, days, 2)
	assert.Equal(t, "2026-06-23", days[0].DiaryDay.Day)
	assert.Equal(t, "2026-06-27", days[1].DiaryDay.Day)
	require.Len(t, days[1].Blocks, 2)
	assert.Equal(t, "the ninety-second sky", *days[1].Blocks[1].Caption)

	// The snapshot is a copy: editing it leaves the journal alone.
	removed, err := s.AutosaveDiaryDay(ctx, days[0].DiaryDay.ID,
		[]DiaryBlock{{Type: "text", Content: strPtr("gulmohar, trimmed")}})
	require.NoError(t, err)
	assert.False(t, removed)

	journal, err := s.GetRambleByDay(ctx, user, "2026-06-23")
	require.NoError(t, err)
	blocks, err := s.ListRambleBlocks(ctx, journal.ID)
	require.NoError(t, err)
	assert.Equal(t, "gulmohar", *blocks[0].Content)

	// Shared upload paths are reference-counted across journal + snapshots.
	refs, err := s.CountUploadsReferencing(ctx, "/up/2026/06/sky.jpg")
	require.NoError(t, err)
	assert.Equal(t, 2, refs)
}

func TestDiaryRefreshSkipsTrimmedDays(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Meera", "meera@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-07-01",
		[]RambleBlock{{Type: "text", Content: strPtr("day one")}})
	require.NoError(t, err)

	sectionID, copied, err := s.AttachDiarySection(ctx, issueID, user, "", "2026-07-02")
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	// The member trims the day out of the review…
	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	_, err = s.DeleteDiaryDay(ctx, days[0].DiaryDay.ID)
	require.NoError(t, err)

	// …then rambles two more days and pulls in the new ones.
	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-07-03",
		[]RambleBlock{{Type: "text", Content: strPtr("day three")}})
	require.NoError(t, err)
	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-07-05",
		[]RambleBlock{{Type: "text", Content: strPtr("day five")}})
	require.NoError(t, err)

	added, err := s.RefreshDiarySection(ctx, sectionID, user, "2026-07-06")
	require.NoError(t, err)
	assert.Equal(t, 2, added)

	// The trimmed 2026-07-01 never reappears.
	days, err = s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	require.Len(t, days, 2)
	assert.Equal(t, "2026-07-03", days[0].DiaryDay.Day)
	assert.Equal(t, "2026-07-05", days[1].DiaryDay.Day)

	// A refresh with no new days is a no-op.
	added, err = s.RefreshDiarySection(ctx, sectionID, user, "2026-07-06")
	require.NoError(t, err)
	assert.Zero(t, added)
}

func TestDiaryRefreshPullsSameDayPage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Meera", "meera@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	// Attach with an empty journal, then write a page later the same day.
	sectionID, copied, err := s.AttachDiarySection(ctx, issueID, user, "", "2026-07-04")
	require.NoError(t, err)
	require.Zero(t, copied)

	_, _, err = s.AutosaveRambleDay(ctx, user, "2026-07-04",
		[]RambleBlock{{Type: "text", Content: strPtr("an evening thought")}})
	require.NoError(t, err)

	n, err := s.CountPullableRambleDays(ctx, user, sectionID, "2026-07-04")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	added, err := s.RefreshDiarySection(ctx, sectionID, user, "2026-07-04")
	require.NoError(t, err)
	assert.Equal(t, 1, added)

	// Now present in the section, so no longer pullable.
	n, err = s.CountPullableRambleDays(ctx, user, sectionID, "2026-07-04")
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestDiarySectionListingAndDetach(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	meera := seedUser(t, s, "Meera", "meera@example.com")
	arun := seedUser(t, s, "Arun", "arun@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	for user, day := range map[int64]string{meera: "2026-07-02", arun: "2026-07-03"} {
		_, _, err = s.AutosaveRambleDay(ctx, user, day,
			[]RambleBlock{{Type: "text", Content: strPtr("note")}})
		require.NoError(t, err)

		_, _, err = s.AttachDiarySection(ctx, issueID, user, "", "2026-07-05")
		require.NoError(t, err)
	}

	groups, err := s.ListDiarySectionsByIssue(ctx, issueID)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "Arun", groups[0].UserName)
	assert.Equal(t, "Meera", groups[1].UserName)
	require.Len(t, groups[0].Days, 1)

	// Trimming a section to zero days hides it from the spread.
	removed, err := s.AutosaveDiaryDay(ctx, groups[0].Days[0].DiaryDay.ID, nil)
	require.NoError(t, err)
	assert.True(t, removed)

	groups, err = s.ListDiarySectionsByIssue(ctx, issueID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "Meera", groups[0].UserName)

	// Detach removes the section entirely.
	section, err := s.GetDiarySection(ctx, issueID, meera)
	require.NoError(t, err)
	_, err = s.DeleteDiarySection(ctx, section.ID)
	require.NoError(t, err)

	groups, err = s.ListDiarySectionsByIssue(ctx, issueID)
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestDiaryDayComments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	meera := seedUser(t, s, "Meera", "meera@example.com")
	arun := seedUser(t, s, "Arun", "arun@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	_, _, err = s.AutosaveRambleDay(ctx, meera, "2026-07-05",
		[]RambleBlock{{Type: "text", Content: strPtr("the samosa place changed hands")}})
	require.NoError(t, err)

	sectionID, _, err := s.AttachDiarySection(ctx, issueID, meera, "", "2026-07-06")
	require.NoError(t, err)

	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	dayID := days[0].DiaryDay.ID

	top, err := s.CreateDiaryComment(ctx, arun, dayID, nil, "new owners or new menu?")
	require.NoError(t, err)
	_, err = s.CreateDiaryComment(ctx, meera, dayID, &top, "new owners. the chutney is FINE.")
	require.NoError(t, err)

	comments, err := s.ListCommentsByDiaryDay(ctx, dayID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Equal(t, "Arun", comments[0].AuthorName)
	require.NotNil(t, comments[0].DiaryDayID)
	assert.Nil(t, comments[0].ResponseID)
	require.NotNil(t, comments[1].ParentID)
	assert.Equal(t, top, *comments[1].ParentID)

	// The comment resolves back to its issue for the Loop guard.
	issue, err := s.GetIssueByDiaryDayID(ctx, dayID)
	require.NoError(t, err)
	assert.Equal(t, issueID, issue.ID)

	// Response comments still work unchanged alongside.
	qID, err := s.CreateQuestion(ctx, issueID, "A question?", nil, "bank", nil, 0)
	require.NoError(t, err)
	respID, err := s.CreateResponse(ctx, meera, qID)
	require.NoError(t, err)
	_, err = s.CreateComment(ctx, arun, respID, nil, "lovely answer")
	require.NoError(t, err)

	respComments, err := s.ListCommentsByResponse(ctx, respID)
	require.NoError(t, err)
	require.Len(t, respComments, 1)
	require.NotNil(t, respComments[0].ResponseID)
	assert.Nil(t, respComments[0].DiaryDayID)
}
