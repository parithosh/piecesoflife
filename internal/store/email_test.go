package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeginSchedulerEmailAttempt_SkipsSentRetry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	userID := seedUser(t, s, "Dana", "dana@example.com")
	eventID := seedSchedulerEvent(t, s, "reminder_1")

	logID, shouldSend, err := s.BeginSchedulerEmailAttempt(
		ctx, eventID, userID, nil, nil, "reminder",
	)
	require.NoError(t, err)
	require.True(t, shouldSend)
	require.NotZero(t, logID)

	now := time.Now()
	require.NoError(t, s.UpdateEmailLog(ctx, logID, "sent", &now, nil))

	retryLogID, shouldSend, err := s.BeginSchedulerEmailAttempt(
		ctx, eventID, userID, nil, nil, "reminder",
	)
	require.NoError(t, err)

	assert.Equal(t, logID, retryLogID)
	assert.False(t, shouldSend, "sent scheduler email should not be sent again")
}

func TestBeginSchedulerEmailAttempt_RetriesFailedAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	userID := seedUser(t, s, "Eli", "eli@example.com")
	eventID := seedSchedulerEvent(t, s, "reminder_1")

	logID, shouldSend, err := s.BeginSchedulerEmailAttempt(
		ctx, eventID, userID, nil, nil, "reminder",
	)
	require.NoError(t, err)
	require.True(t, shouldSend)

	errText := "smtp unavailable"
	require.NoError(t, s.UpdateEmailLog(ctx, logID, "failed", nil, &errText))

	retryLogID, shouldSend, err := s.BeginSchedulerEmailAttempt(
		ctx, eventID, userID, nil, nil, "reminder",
	)
	require.NoError(t, err)
	require.True(t, shouldSend, "failed scheduler email should be retried")
	assert.Equal(t, logID, retryLogID)

	logEntry, err := s.GetEmailLogByID(ctx, retryLogID)
	require.NoError(t, err)
	assert.Equal(t, "pending", logEntry.Status)
	assert.Nil(t, logEntry.Error)
}

func seedSchedulerEvent(t *testing.T, s *Store, eventType string) int64 {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, s.CreateSchedulerEvent(
		ctx, nil, eventType, time.Now().Add(-time.Minute),
	))

	events, err := s.GetPendingEvents(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, events)

	return events[len(events)-1].ID
}
