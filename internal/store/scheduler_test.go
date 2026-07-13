package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnsureDailyEventDedupes pins the fix for duplicate daily events:
// UNIQUE(issue_id, event_type, scheduled_at) never held for issue-less rows
// because SQLite treats NULLs as distinct inside UNIQUE constraints, so the
// old plain INSERT stacked a duplicate on every scheduler tick. Dedupe now
// comes from EnsureDailyEvent's INSERT OR IGNORE against the partial unique
// index added in migration 020.
func TestEnsureDailyEventDedupes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	midnight := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

	for range 3 {
		require.NoError(t, s.EnsureDailyEvent(ctx, "comment_digest", midnight))
	}

	assert.Equal(t, 1, countPendingByType(t, s, "comment_digest"),
		"repeat scheduling must not stack duplicate daily events")

	// A different midnight is a different event — the index must not
	// collapse consecutive days into one.
	require.NoError(t, s.EnsureDailyEvent(
		ctx, "comment_digest", midnight.AddDate(0, 0, 1)))

	assert.Equal(t, 2, countPendingByType(t, s, "comment_digest"),
		"the next day's event must still be schedulable")
}

// countPendingByType counts unfired scheduler events of one type.
func countPendingByType(t *testing.T, s *Store, eventType string) int {
	t.Helper()

	pending, err := s.GetPendingEvents(context.Background())
	require.NoError(t, err)

	count := 0

	for _, ev := range pending {
		if ev.EventType == eventType {
			count++
		}
	}

	return count
}
