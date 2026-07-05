package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestStore returns a fully-migrated Store backed by a fresh SQLite file
// in the test's temp dir. Using a file rather than :memory: keeps the two
// connection pools (read + write) talking to the same database, which
// matches production behaviour. Cleanup is automatic.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "test.db")

	s, err := New(ctx, path, logger)
	require.NoError(t, err, "open test store")

	require.NoError(t, s.RunMigrations(ctx), "run migrations")
	require.NoError(t, s.SeedDefaultGroup(ctx), "seed default group")

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// seedUser inserts an active member of group 1 and returns the new user's ID.
func seedUser(t *testing.T, s *Store, name, email string) int64 {
	t.Helper()
	ctx := context.Background()

	id, err := s.CreateUser(ctx, name, email)
	require.NoError(t, err, "create user")

	require.NoError(t, s.CreateMembership(ctx, 1, id, "member"), "create membership")

	return id
}
