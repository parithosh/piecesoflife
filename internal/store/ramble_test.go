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

// seedRambleText creates a journal day holding one text block per given
// string — the seeding path the create handler uses (CreateRambleTextBlock).
func seedRambleText(
	t *testing.T, s *Store, userID int64, day string, texts ...string,
) int64 {
	t.Helper()
	ctx := context.Background()

	for _, txt := range texts {
		_, err := s.CreateRambleTextBlock(ctx, userID, day, txt)
		require.NoError(t, err)
	}

	id, err := s.EnsureRambleDay(ctx, userID, day)
	require.NoError(t, err)

	return id
}

func TestRambleBlockLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Meera", "meera@example.com")

	id := seedRambleText(t, s, user, "2026-06-23", "The gulmohar flowered.")

	blocks, err := s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 1)

	// Editing a block replaces its content in place.
	blockRemoved, dayRemoved, err := s.UpdateRambleBlockText(
		ctx, blocks[0].ID, "The gulmohar finally flowered.")
	require.NoError(t, err)
	assert.False(t, blockRemoved)
	assert.False(t, dayRemoved)

	blocks, err = s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	assert.Equal(t, "The gulmohar finally flowered.", *blocks[0].Content)

	// A second thought is its own block, ordered after the first.
	secondID, err := s.CreateRambleBlock(ctx, id, "text",
		strPtr("And the crows noticed."), nil, nil)
	require.NoError(t, err)

	blocks, err = s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 2)
	assert.Equal(t, "And the crows noticed.", *blocks[1].Content)
	assert.Greater(t, blocks[1].SortOrder, blocks[0].SortOrder)

	// Media blocks live in the same sequence.
	photoID, err := s.CreateRambleBlock(ctx, id, "photo", nil,
		strPtr("/up/2026/06/x.jpg"), nil)
	require.NoError(t, err)

	// Only text blocks can be edited.
	_, _, err = s.UpdateRambleBlockText(ctx, photoID, "sneaky caption")
	assert.Error(t, err)

	// Emptying a block deletes it; the day survives while siblings remain.
	blockRemoved, dayRemoved, err = s.UpdateRambleBlockText(ctx, blocks[0].ID, "")
	require.NoError(t, err)
	assert.True(t, blockRemoved)
	assert.False(t, dayRemoved)

	blocks, err = s.ListRambleBlocks(ctx, id)
	require.NoError(t, err)
	require.Len(t, blocks, 2)

	// Deleting the remaining text and media empties the day out of existence.
	blockRemoved, dayRemoved, err = s.UpdateRambleBlockText(ctx, secondID, "")
	require.NoError(t, err)
	assert.True(t, blockRemoved)
	assert.False(t, dayRemoved)

	require.NoError(t, s.DeleteRambleBlock(ctx, photoID))

	_, err = s.GetRambleByDay(ctx, user, "2026-06-23")
	assert.Error(t, err, "empty day should be invisible")

	// Emptying the last block removes the day with it.
	id2 := seedRambleText(t, s, user, "2026-06-24", "a lone thought")
	blocks, err = s.ListRambleBlocks(ctx, id2)
	require.NoError(t, err)

	blockRemoved, dayRemoved, err = s.UpdateRambleBlockText(ctx, blocks[0].ID, "")
	require.NoError(t, err)
	assert.True(t, blockRemoved)
	assert.True(t, dayRemoved)

	days, err := s.ListRambleDays(ctx, user)
	require.NoError(t, err)
	assert.Empty(t, days)

	// A vanished block can't be edited.
	_, _, err = s.UpdateRambleBlockText(ctx, blocks[0].ID, "too late")
	assert.Error(t, err)
}

func TestRambleDayListingAndCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	user := seedUser(t, s, "Arun", "arun@example.com")
	other := seedUser(t, s, "Zara", "zara@example.com")

	for _, day := range []string{"2026-06-19", "2026-06-29", "2026-07-08"} {
		seedRambleText(t, s, user, day, "note on "+day)
	}

	seedRambleText(t, s, other, "2026-07-01", "someone else's note")

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

	// Two separate thoughts on one day stay two separate blocks.
	seedRambleText(t, s, user, "2026-06-23", "gulmohar", "crows again")

	rambleID := seedRambleText(t, s, user, "2026-06-27", "evening walk")
	_, err = s.CreateRambleBlock(ctx, rambleID, "photo", nil,
		strPtr("/up/2026/06/sky.jpg"), strPtr("the ninety-second sky"))
	require.NoError(t, err)

	// A day outside the window stays out of the snapshot.
	seedRambleText(t, s, user, "2026-05-01", "too old")

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
	require.Len(t, days[0].Blocks, 2, "each journal block copies separately")
	require.Len(t, days[1].Blocks, 2)
	assert.Equal(t, "the ninety-second sky", *days[1].Blocks[1].Caption)

	// The snapshot is a copy: editing it leaves the journal alone.
	blockRemoved, dayRemoved, err := s.UpdateDiaryBlockText(
		ctx, days[0].Blocks[0].ID, "gulmohar, trimmed")
	require.NoError(t, err)
	assert.False(t, blockRemoved)
	assert.False(t, dayRemoved)

	journal, err := s.GetRambleByDay(ctx, user, "2026-06-23")
	require.NoError(t, err)
	blocks, err := s.ListRambleBlocks(ctx, journal.ID)
	require.NoError(t, err)
	assert.Equal(t, "gulmohar", *blocks[0].Content)

	// Trimming one snapshot block leaves its siblings in the spread.
	blockRemoved, dayRemoved, err = s.UpdateDiaryBlockText(
		ctx, days[0].Blocks[1].ID, "")
	require.NoError(t, err)
	assert.True(t, blockRemoved)
	assert.False(t, dayRemoved)

	days, err = s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	require.Len(t, days[0].Blocks, 1)
	assert.Equal(t, "gulmohar, trimmed", *days[0].Blocks[0].Content)

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

	seedRambleText(t, s, user, "2026-07-01", "day one")

	sectionID, copied, err := s.AttachDiarySection(ctx, issueID, user, "", "2026-07-02")
	require.NoError(t, err)
	require.Equal(t, 1, copied)

	// The member trims the day out of the review…
	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	_, err = s.DeleteDiaryDay(ctx, days[0].DiaryDay.ID)
	require.NoError(t, err)

	// …then rambles two more days and pulls in the new ones.
	seedRambleText(t, s, user, "2026-07-03", "day three")
	seedRambleText(t, s, user, "2026-07-05", "day five")

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

	seedRambleText(t, s, user, "2026-07-04", "an evening thought")

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
		seedRambleText(t, s, user, day, "note")

		_, _, err = s.AttachDiarySection(ctx, issueID, user, "", "2026-07-05")
		require.NoError(t, err)
	}

	groups, err := s.ListDiarySectionsByIssue(ctx, issueID)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "Arun", groups[0].UserName)
	assert.Equal(t, "Meera", groups[1].UserName)
	require.Len(t, groups[0].Days, 1)

	// Trimming a section's every block hides it from the spread.
	blockRemoved, dayRemoved, err := s.UpdateDiaryBlockText(
		ctx, groups[0].Days[0].Blocks[0].ID, "")
	require.NoError(t, err)
	assert.True(t, blockRemoved)
	assert.True(t, dayRemoved)

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

	seedRambleText(t, s, meera, "2026-07-05", "the samosa place changed hands")

	sectionID, _, err := s.AttachDiarySection(ctx, issueID, meera, "", "2026-07-06")
	require.NoError(t, err)

	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	dayID := days[0].DiaryDay.ID

	// Legacy day-level threads (pre-021 issues) still work.
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
	assert.Nil(t, comments[0].DiaryBlockID)
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

func TestDiaryBlockComments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	meera := seedUser(t, s, "Meera", "meera@example.com")
	arun := seedUser(t, s, "Arun", "arun@example.com")

	issueID, err := s.CreateIssue(ctx, 1, nil, 7, 2026,
		mustParseTime(t, "2026-07-01"), mustParseTime(t, "2026-07-08"))
	require.NoError(t, err)

	seedRambleText(t, s, meera, "2026-07-05",
		"the samosa place changed hands", "and the mango cart is back")

	sectionID, _, err := s.AttachDiarySection(ctx, issueID, meera, "", "2026-07-06")
	require.NoError(t, err)

	days, err := s.ListDiaryDays(ctx, sectionID)
	require.NoError(t, err)
	require.Len(t, days[0].Blocks, 2)

	first := days[0].Blocks[0].ID
	second := days[0].Blocks[1].ID

	// A thread lands on one block, not its neighbour and not the day.
	top, err := s.CreateDiaryBlockComment(ctx, arun, second, nil, "WHICH mango cart?")
	require.NoError(t, err)
	_, err = s.CreateDiaryBlockComment(ctx, meera, second, &top, "the good one. obviously.")
	require.NoError(t, err)

	comments, err := s.ListCommentsByDiaryBlock(ctx, second)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	assert.Equal(t, "Arun", comments[0].AuthorName)
	require.NotNil(t, comments[0].DiaryBlockID)
	assert.Equal(t, second, *comments[0].DiaryBlockID)
	assert.Nil(t, comments[0].DiaryDayID)
	assert.Nil(t, comments[0].ResponseID)
	require.NotNil(t, comments[1].ParentID)
	assert.Equal(t, top, *comments[1].ParentID)

	neighbour, err := s.ListCommentsByDiaryBlock(ctx, first)
	require.NoError(t, err)
	assert.Empty(t, neighbour)

	// The issue page counts every block's thread in one grouped query;
	// blocks without comments are simply absent.
	counts, err := s.CountCommentsByDiaryBlocks(ctx, issueID)
	require.NoError(t, err)
	assert.Equal(t, map[int64]int{second: 2}, counts)

	dayComments, err := s.ListCommentsByDiaryDay(ctx, days[0].DiaryDay.ID)
	require.NoError(t, err)
	assert.Empty(t, dayComments)

	// The block resolves back to its issue for the Loop guard.
	issue, err := s.GetIssueByDiaryBlockID(ctx, second)
	require.NoError(t, err)
	assert.Equal(t, issueID, issue.ID)

	// Trimming a commented block takes its thread with it (CASCADE).
	require.NoError(t, s.DeleteDiaryBlock(ctx, second))

	comments, err = s.ListCommentsByDiaryBlock(ctx, second)
	require.NoError(t, err)
	assert.Empty(t, comments)

	_, err = s.GetCommentByID(ctx, top)
	assert.Error(t, err)
}
