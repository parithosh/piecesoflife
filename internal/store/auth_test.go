package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConsumeAuthToken_Atomicity verifies that two concurrent consumers of
// the same token only succeed once — the fix I made for the magic-link
// race must actually hold under load.
func TestConsumeAuthToken_Atomicity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	userID := seedUser(t, s, "Alice", "alice@example.com")

	expiresAt := time.Now().Add(30 * time.Minute)
	require.NoError(t, s.CreateAuthToken(ctx, userID, "hash-abc", "login", expiresAt))

	tok, err := s.GetAuthTokenByHash(ctx, "hash-abc")
	require.NoError(t, err)

	const concurrency = 10

	var (
		wg        sync.WaitGroup
		successes int32
		raceWins  int32
		mu        sync.Mutex
	)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := s.ConsumeAuthToken(ctx, tok.ID)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrTokenAlreadyConsumed):
				raceWins++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	assert.Equal(t, int32(1), successes, "exactly one consumer should win the race")
	assert.Equal(t, int32(concurrency-1), raceWins, "the rest should all see ErrTokenAlreadyConsumed")
}

func TestConsumeAuthToken_UnknownID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.ConsumeAuthToken(ctx, 9999)
	// Unknown id: UPDATE affects 0 rows — treated as "already consumed"
	// from the caller's perspective. This is intentional; an attacker
	// guessing IDs gets the same response shape as a race loser.
	assert.ErrorIs(t, err, ErrTokenAlreadyConsumed)
}
