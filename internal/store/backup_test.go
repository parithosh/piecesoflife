package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBackupBeforeMigration verifies the pre-upgrade snapshot: a consistent
// single-file copy appears next to the live database and is itself a valid
// database with the same applied-migration history.
func TestBackupBeforeMigration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedUser(t, s, "Backup", "backup@example.com")

	backupPath, err := s.backupBeforeMigration(ctx, 999)
	require.NoError(t, err, "take backup")

	require.FileExists(t, backupPath)
	assert.True(t, strings.Contains(filepath.Base(backupPath), "backup-before-999-"),
		"backup name carries the pending version")

	// The snapshot must be a usable database: open it and compare the
	// migration history and the seeded user.
	restored, err := New(ctx, backupPath, testLogger())
	require.NoError(t, err, "open backup as a database")

	t.Cleanup(func() { _ = restored.Close() })

	var versions int
	require.NoError(t, restored.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations").Scan(&versions))
	assert.Greater(t, versions, 0, "backup carries migration history")

	user, err := restored.GetUserByEmail(ctx, "backup@example.com")
	require.NoError(t, err, "backup carries data")
	assert.Equal(t, "Backup", user.Name)
}

// TestRunMigrationsFreshDatabaseTakesNoBackup verifies a brand-new database
// (nothing applied yet) migrates without leaving a backup file behind.
func TestRunMigrationsFreshDatabaseTakesNoBackup(t *testing.T) {
	s := newTestStore(t)

	dir := filepath.Dir(s.dbPath)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".backup-before-",
			"fresh install must not snapshot an empty database")
	}
}
