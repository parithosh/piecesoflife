package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestVerifyDataDirectoryBaseline covers the first boot of an install that
// predates the guard: no sidecar exists, so nothing may fail.
func TestVerifyDataDirectoryBaseline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	_, err := os.Stat(st.statePath())
	require.True(t, os.IsNotExist(err), "no state file before the first record")

	require.NoError(t, st.VerifyDataDirectory(ctx, false))
	require.NoError(t, st.RecordDataDirectoryState(ctx))

	// Same database, second boot: still fine.
	require.NoError(t, st.VerifyDataDirectory(ctx, false))
}

// TestVerifyDataDirectoryDetectsRollback is the 2026-08-05 incident in
// miniature: the data directory has seen 21 migrations, the database file
// carries fewer. Starting up must fail rather than migrate it forward.
func TestVerifyDataDirectoryDetectsRollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	require.NoError(t, st.RecordDataDirectoryState(ctx))

	applied, _, err := st.MigrationState(ctx)
	require.NoError(t, err)
	require.Positive(t, applied, "migrations applied by the test store")

	// Rewind the database the way a stale restore does, leaving the
	// sidecar reflecting the newer state.
	_, err = st.write.ExecContext(ctx,
		"DELETE FROM schema_migrations WHERE version >= (SELECT MAX(version) FROM schema_migrations)")
	require.NoError(t, err)

	err = st.VerifyDataDirectory(ctx, false)
	require.Error(t, err, "rollback must fail closed")

	var rollback *ErrDatabaseRolledBack
	require.ErrorAs(t, err, &rollback)
	require.Equal(t, applied, rollback.SidecarAppliedCount)
	require.Less(t, rollback.DatabaseAppliedCount, rollback.SidecarAppliedCount)

	// The override exists for operators restoring a backup on purpose.
	require.NoError(t, st.VerifyDataDirectory(ctx, true),
		"ALLOW_DB_ROLLBACK must downgrade the failure to a warning")
}

// TestVerifyDataDirectoryDetectsNewerDatabase covers the mirror case: an
// app downgrade against a database a later release already upgraded.
func TestVerifyDataDirectoryDetectsNewerDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	highest, err := highestEmbeddedMigration()
	require.NoError(t, err)

	_, err = st.write.ExecContext(ctx,
		"INSERT INTO schema_migrations (version) VALUES (?)", highest+1)
	require.NoError(t, err)

	err = st.VerifyDataDirectory(ctx, false)
	require.Error(t, err)

	var tooNew *ErrDatabaseTooNew
	require.ErrorAs(t, err, &tooNew)
	require.Equal(t, highest+1, tooNew.DatabaseVersion)
	require.Equal(t, highest, tooNew.BinaryVersion)

	// Not a rollback, so the rollback override must not wave it through.
	require.Error(t, st.VerifyDataDirectory(ctx, true),
		"ALLOW_DB_ROLLBACK must not excuse a too-new database")
}

// TestRecordDataDirectoryStateIsReadable keeps the sidecar debuggable by a
// human staring at a broken deploy.
func TestRecordDataDirectoryStateIsReadable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newTestStore(t)

	require.NoError(t, st.RecordDataDirectoryState(ctx))

	raw, err := os.ReadFile(st.statePath())
	require.NoError(t, err)

	var state dataDirState
	require.NoError(t, json.Unmarshal(raw, &state))

	applied, version, err := st.MigrationState(ctx)
	require.NoError(t, err)

	require.Equal(t, applied, state.AppliedCount)
	require.Equal(t, version, state.SchemaVersion)
	require.WithinDuration(t, time.Now(), state.UpdatedAt, time.Minute)
}
