package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetLastFiredAtReturnsNilWhenNoEventsFired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	last, err := s.GetLastFiredAt(ctx, "token_cleanup")

	require.NoError(t, err)
	assert.Nil(t, last)
}

func TestGetLastFiredAtParsesSQLiteTimestampText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.write.ExecContext(ctx,
		`INSERT INTO scheduler_events
		 (event_type, scheduled_at, fired_at)
		 VALUES (?, ?, ?), (?, ?, ?)`,
		"token_cleanup", "2026-06-15 20:22:10", "2026-06-15 20:22:10",
		"token_cleanup", "2026-06-16 20:22:10", "2026-06-16 20:22:10",
	)
	require.NoError(t, err)

	last, err := s.GetLastFiredAt(ctx, "token_cleanup")

	require.NoError(t, err)
	require.NotNil(t, last)

	expected := time.Date(2026, 6, 16, 20, 22, 10, 0, time.UTC)
	assert.True(t, last.Equal(expected), "expected %s, got %s", expected, last)
}
