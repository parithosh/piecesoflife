package store

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerifyDataDirectoryBaseline covers two cases at once: the first boot
// of an install that predates the guard, and the documented escape hatch of
// deleting the sidecar to accept an older database. Both look identical —
// no state file — and neither may fail.
func TestVerifyDataDirectoryBaseline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	_, err := os.Stat(st.statePath())
	require.True(t, os.IsNotExist(err), "no state file before the first record")

	require.NoError(t, st.VerifyDataDirectory(ctx))
	require.NoError(t, st.RecordDataDirectoryState(ctx))

	// Same database, second boot: still fine.
	require.NoError(t, st.VerifyDataDirectory(ctx))

	// Escape hatch: the sidecar is gone, so the next boot re-baselines.
	require.NoError(t, os.Remove(st.statePath()))
	require.NoError(t, st.VerifyDataDirectory(ctx))
}

// TestVerifyDataDirectoryDetectsRollback is the 2026-08-05 incident in
// miniature. It deliberately rewinds ONLY the generation, leaving the schema
// untouched, because that is the case a migration-count comparison cannot
// see and the one that loses data without any other symptom.
func TestVerifyDataDirectoryDetectsRollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	for range 3 {
		require.NoError(t, st.VerifyDataDirectory(ctx))
		require.NoError(t, st.RecordDataDirectoryState(ctx))
	}

	generation, err := st.DataGeneration(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, generation, "one generation per boot")

	appliedBefore, _, err := st.migrationState(ctx)
	require.NoError(t, err)

	// Restore a snapshot taken two boots ago: identical schema, older data.
	require.NoError(t, st.setDataGeneration(ctx, 1))

	appliedAfter, _, err := st.migrationState(ctx)
	require.NoError(t, err)
	require.Equal(t, appliedBefore, appliedAfter,
		"schema is unchanged, so only the generation can detect this")

	err = st.VerifyDataDirectory(ctx)
	require.Error(t, err, "a rewound database must fail closed")
	require.ErrorContains(t, err, "boot generation 1")
	require.ErrorContains(t, err, "last saw 3")
	require.ErrorContains(t, err, st.statePath(),
		"the message must name the file an operator can delete")
}

// TestVerifyDataDirectoryAcceptsGenerationAhead covers a crash between the
// header write and the sidecar write: the database is ahead, which is not a
// rollback and must not block startup.
func TestVerifyDataDirectoryAcceptsGenerationAhead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	require.NoError(t, st.RecordDataDirectoryState(ctx))
	require.NoError(t, st.setDataGeneration(ctx, 99))

	require.NoError(t, st.VerifyDataDirectory(ctx),
		"a database ahead of its sidecar is not a rollback")
}
