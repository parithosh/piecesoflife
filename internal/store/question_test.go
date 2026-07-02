package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteQuestionsByIssue locks in the onboarding-retry behaviour: the
// wizard wipes questions when re-submitted, so this helper must actually
// remove rows for the target issue (and only that issue).
func TestDeleteQuestionsByIssue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	issueA, err := s.CreateIssue(ctx, nil, 4, 2026,
		time.Now(), time.Now().Add(7*24*time.Hour))
	require.NoError(t, err)

	issueB, err := s.CreateIssue(ctx, nil, 5, 2026,
		time.Now(), time.Now().Add(7*24*time.Hour))
	require.NoError(t, err)

	// Three questions on A, two on B.
	for i, text := range []string{"q1", "q2", "q3"} {
		_, err := s.CreateQuestion(ctx, issueA, text, nil, "bank", nil, i)
		require.NoError(t, err)
	}
	for i, text := range []string{"p1", "p2"} {
		_, err := s.CreateQuestion(ctx, issueB, text, nil, "bank", nil, i)
		require.NoError(t, err)
	}

	require.NoError(t, s.DeleteQuestionsByIssue(ctx, issueA))

	qsA, err := s.ListQuestionsByIssue(ctx, issueA)
	require.NoError(t, err)
	assert.Empty(t, qsA, "issue A should have no questions after delete")

	qsB, err := s.ListQuestionsByIssue(ctx, issueB)
	require.NoError(t, err)
	assert.Len(t, qsB, 2, "issue B should be untouched")
}

// TestSelectRandomUnusedQuestions_CategorySpread verifies the round-robin
// selection produces varied categories when the bank has enough entries.
func TestSelectRandomUnusedQuestions_CategorySpread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	categories := []string{"life_updates", "deep_thoughts", "fun_silly",
		"memories", "goals", "recommendations", "hypotheticals"}

	for _, cat := range categories {
		for i := 0; i < 5; i++ {
			_, err := s.CreateBankQuestion(ctx, cat+"-"+string(rune('a'+i)), cat)
			require.NoError(t, err)
		}
	}

	picked, err := s.SelectRandomUnusedQuestions(ctx, 5)
	require.NoError(t, err)
	require.Len(t, picked, 5)

	seen := make(map[string]bool, 5)
	for _, q := range picked {
		seen[q.Category] = true
	}

	assert.GreaterOrEqual(t, len(seen), 3,
		"with 7 categories and picking 5 we expect at least 3 distinct categories")
}
