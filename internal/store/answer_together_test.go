package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hotSeat is a collecting issue with three hot-seat questions for Ashwin,
// in sort order, and one for someone else.
type hotSeat struct {
	s              *Store
	issueID        int64
	ashwin, other  int64
	q1, q2, q3, qx int64
}

func newHotSeat(t *testing.T) *hotSeat {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	h := &hotSeat{s: s,
		ashwin: seedUser(t, s, "Ashwin", "ashwin@example.com"),
		other:  seedUser(t, s, "Megha", "megha@example.com"),
	}

	now := time.Now().UTC()
	var err error
	h.issueID, err = s.CreateIssue(ctx, 1, nil, int(now.Month()), now.Year(), now, now.Add(7*24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, s.SetIssueStatus(ctx, h.issueID, "collecting"))

	add := func(text string, target int64, order int) int64 {
		res, err := s.write.ExecContext(ctx,
			`INSERT INTO questions (issue_id, text, source, target_user_id, sort_order)
			 VALUES (?, ?, 'friend', ?, ?)`, h.issueID, text, target, order)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}
	h.q1 = add("How was your birthday? More 3D-printed toys?", h.ashwin, 0)
	h.q2 = add("How was your birthday? Did you celebrate with Megha?", h.ashwin, 1)
	h.q3 = add("What are you reading?", h.ashwin, 2)
	h.qx = add("How is the new job?", h.other, 3)

	return h
}

// answer gives q a response, with text when text != "", submitted or not.
func (h *hotSeat) answer(t *testing.T, q int64, text string, submitted bool) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := h.s.CreateResponse(ctx, h.ashwin, q)
	require.NoError(t, err)
	if text != "" {
		_, err = h.s.AutosaveResponse(ctx, id, 1, []ResponseBlock{{Type: "text", Content: &text}})
		require.NoError(t, err)
	}
	if submitted {
		require.NoError(t, h.s.SubmitResponse(ctx, id))
	}
	return id
}

func (h *hotSeat) answeredWith(t *testing.T, q int64) *int64 {
	t.Helper()
	got, err := h.s.GetQuestionByID(context.Background(), q)
	require.NoError(t, err)
	return got.AnsweredWith
}

func TestAnswerTogetherPrimary(t *testing.T) {
	ctx := context.Background()

	t.Run("without answers the earlier question is primary, either way round", func(t *testing.T) {
		h := newHotSeat(t)
		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q1})
		require.NoError(t, err)
		assert.Equal(t, h.q1, primary)
		assert.Equal(t, &h.q1, h.answeredWith(t, h.q2))
		assert.Nil(t, h.answeredWith(t, h.q1))
	})

	t.Run("the answered question is primary even when it is later", func(t *testing.T) {
		h := newHotSeat(t)
		resp := h.answer(t, h.q2, "Megha baked a cake", false)
		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q1})
		require.NoError(t, err)
		assert.Equal(t, h.q2, primary)
		got, err := h.s.GetResponseByID(ctx, resp)
		require.NoError(t, err)
		assert.Equal(t, h.q2, got.QuestionID, "the answer stays where it was")
	})

	t.Run("two answers with content are never merged", func(t *testing.T) {
		h := newHotSeat(t)
		h.answer(t, h.q1, "Printed a dragon", false)
		h.answer(t, h.q2, "Megha baked a cake", false)
		_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
		assert.ErrorIs(t, err, ErrBothAnswered)
		assert.Nil(t, h.answeredWith(t, h.q2))
	})
}

// Submit creates and submits a blank row for every untouched card, so a
// submitted row alone must not count as an answer. Combining never submits
// a draft, and never reuses a response ID a stale tab may still write to.
func TestAnswerTogetherAfterSubmit(t *testing.T) {
	ctx := context.Background()

	t.Run("blank submitted row is dropped; the draft answer stays a draft", func(t *testing.T) {
		h := newHotSeat(t)
		answered := h.answer(t, h.q2, "Megha baked a cake", false)
		blank := h.answer(t, h.q1, "", true)

		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
		require.NoError(t, err)
		assert.Equal(t, h.q2, primary)

		_, err = h.s.GetResponseByID(ctx, blank)
		assert.Error(t, err, "the blank row is gone")
		got, err := h.s.GetResponseByID(ctx, answered)
		require.NoError(t, err)
		assert.True(t, got.IsDraft, "combining is not consent to submit")
	})

	t.Run("a primary without a row gets a fresh blank one in the same state", func(t *testing.T) {
		h := newHotSeat(t)
		blank := h.answer(t, h.q2, "", true)

		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q1})
		require.NoError(t, err)
		assert.Equal(t, h.q1, primary)

		_, err = h.s.GetResponseByID(ctx, blank)
		assert.Error(t, err, "a stale tab writing to the old ID must miss")
		got, err := h.s.GetUserResponse(ctx, h.ashwin, h.q1)
		require.NoError(t, err)
		assert.NotEqual(t, blank, got.ID)
		assert.False(t, got.IsDraft, "the member's submitted state is unchanged")
	})

	t.Run("whitespace-only text is not an answer", func(t *testing.T) {
		h := newHotSeat(t)
		h.answer(t, h.q1, "Printed a dragon", false)
		h.answer(t, h.q2, "\n\t ", false)
		_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
		assert.NoError(t, err)
	})
}

func TestAnswerTogetherGuards(t *testing.T) {
	ctx := context.Background()
	h := newHotSeat(t)

	_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.qx})
	assert.ErrorIs(t, err, ErrQuestionNotYours, "another member's question")
	_, err = h.s.AnswerTogether(ctx, 2, h.ashwin, []int64{h.q1, h.q2})
	assert.ErrorIs(t, err, ErrQuestionNotYours, "another Loop")
	_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q1})
	assert.ErrorIs(t, err, ErrStaleGrouping)

	// A joined question can't gain its own answer, even from a stale tab.
	_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
	require.NoError(t, err)
	_, err = h.s.CreateResponse(ctx, h.ashwin, h.q2)
	assert.ErrorIs(t, err, ErrAnsweredTogether)

	// A page still showing q2 on its own can't join it again.
	_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q3, h.q2})
	assert.ErrorIs(t, err, ErrStaleGrouping)
	assert.Equal(t, &h.q1, h.answeredWith(t, h.q2))

	// Separating names the primary the page showed.
	assert.ErrorIs(t, h.s.AnswerSeparately(ctx, 1, h.ashwin, h.q2, h.q3), ErrStaleGrouping)

	require.NoError(t, h.s.SetIssueStatus(ctx, h.issueID, "published"))
	assert.ErrorIs(t, h.s.AnswerSeparately(ctx, 1, h.ashwin, h.q2, h.q1), ErrNotCollecting)
	_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q3})
	assert.ErrorIs(t, err, ErrNotCollecting)
}

func TestAnswerTogetherGroupsAndSeparates(t *testing.T) {
	ctx := context.Background()
	h := newHotSeat(t)

	// q3 joins q2, then q2's group joins q1: still one level deep.
	_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q3})
	require.NoError(t, err)
	resp := h.answer(t, h.q1, "Printed a dragon", false)
	primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q1})
	require.NoError(t, err)
	assert.Equal(t, h.q1, primary)
	assert.Equal(t, &h.q1, h.answeredWith(t, h.q2))
	assert.Equal(t, &h.q1, h.answeredWith(t, h.q3))

	// The dashboard counts the shared answer for every question of the group.
	counts, err := h.s.CountResponsesByQuestion(ctx, h.issueID)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[h.q2])
	assert.Equal(t, 1, counts[h.q3])

	// Separating detaches the prompt; the answer stays with the primary.
	require.NoError(t, h.s.AnswerSeparately(ctx, 1, h.ashwin, h.q3, h.q1))
	assert.Nil(t, h.answeredWith(t, h.q3))
	got, err := h.s.GetResponseByID(ctx, resp)
	require.NoError(t, err)
	assert.Equal(t, h.q1, got.QuestionID)
}

func TestDeletingQuestionsAnsweredTogether(t *testing.T) {
	ctx := context.Background()
	h := newHotSeat(t)
	_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
	require.NoError(t, err)

	// Unanswered: deleting the primary makes the other one separate again.
	require.NoError(t, h.s.DeleteQuestion(ctx, h.q1))
	assert.Nil(t, h.answeredWith(t, h.q2))

	// Answered through its primary: the joined question can't be deleted.
	_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q3})
	require.NoError(t, err)
	h.answer(t, h.q2, "Megha baked a cake", false)
	assert.ErrorIs(t, h.s.DeleteQuestion(ctx, h.q3), ErrQuestionAnswered)
	assert.Equal(t, &h.q2, h.answeredWith(t, h.q3))
}

func TestAnswerTogetherGroupOfThree(t *testing.T) {
	ctx := context.Background()

	t.Run("the answered one is primary and the rest join it", func(t *testing.T) {
		h := newHotSeat(t)
		h.answer(t, h.q3, "Reading Piranesi", false)
		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2, h.q3})
		require.NoError(t, err)
		assert.Equal(t, h.q3, primary)
		assert.Equal(t, &h.q3, h.answeredWith(t, h.q1))
		assert.Equal(t, &h.q3, h.answeredWith(t, h.q2))
	})

	t.Run("blank rows go; a fresh primary row keeps the member submitted", func(t *testing.T) {
		h := newHotSeat(t)
		h.answer(t, h.q2, "", true)
		h.answer(t, h.q3, "", false)
		primary, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q3, h.q2, h.q1})
		require.NoError(t, err)
		assert.Equal(t, h.q1, primary, "earliest when none is answered")
		got, err := h.s.GetUserResponse(ctx, h.ashwin, h.q1)
		require.NoError(t, err)
		assert.False(t, got.IsDraft)
		for _, q := range []int64{h.q2, h.q3} {
			_, err := h.s.GetUserResponse(ctx, h.ashwin, q)
			assert.Error(t, err)
		}
	})

	t.Run("two answered among three is refused and changes nothing", func(t *testing.T) {
		h := newHotSeat(t)
		h.answer(t, h.q1, "Printed a dragon", false)
		h.answer(t, h.q3, "Reading Piranesi", false)
		_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2, h.q3})
		assert.ErrorIs(t, err, ErrBothAnswered)
		assert.Nil(t, h.answeredWith(t, h.q2))
	})

	t.Run("a group joins another group through its root", func(t *testing.T) {
		h := newHotSeat(t)
		_, err := h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q2, h.q3})
		require.NoError(t, err)
		_, err = h.s.AnswerTogether(ctx, 1, h.ashwin, []int64{h.q1, h.q2})
		require.NoError(t, err)
		assert.Equal(t, &h.q1, h.answeredWith(t, h.q2))
		assert.Equal(t, &h.q1, h.answeredWith(t, h.q3))
	})
}
