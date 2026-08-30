package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dataDirStateSuffix names the sidecar file written next to the database
// after every successful migration run. It records the schema high-water
// mark OUTSIDE the database, which is the whole point: a marker stored in
// the database itself is restored — and therefore rewound — along with it.
const dataDirStateSuffix = ".state"

// dataDirState is the sidecar payload. AppliedCount is the authoritative
// comparison field; SchemaVersion and UpdatedAt exist to make the file
// readable by a human staring at a broken deploy at 2am.
type dataDirState struct {
	SchemaVersion int       `json:"schema_version"`
	AppliedCount  int       `json:"applied_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ErrDatabaseRolledBack reports a database file older than the data
// directory it sits in — the fingerprint of a restore from a stale
// snapshot. Callers should refuse to start rather than migrate it forward:
// doing so replays the schema on top of old data and silently strands
// every row written since the snapshot.
type ErrDatabaseRolledBack struct {
	DatabaseAppliedCount int
	SidecarAppliedCount  int
	SidecarUpdatedAt     time.Time
	StatePath            string
}

func (e *ErrDatabaseRolledBack) Error() string {
	return fmt.Sprintf(
		"database has %d applied migrations but this data directory last saw %d (%s): "+
			"the database file is older than the data directory, which usually means it "+
			"was restored from a stale snapshot or copied without its -wal sibling. "+
			"Migrating it forward would strand every row written since then. "+
			"Restore the correct database, or set ALLOW_DB_ROLLBACK=true to proceed "+
			"deliberately (state file: %s)",
		e.DatabaseAppliedCount, e.SidecarAppliedCount,
		e.SidecarUpdatedAt.UTC().Format(time.RFC3339), e.StatePath,
	)
}

// ErrDatabaseTooNew reports a database carrying migrations this binary does
// not know about — an app downgrade against an already-upgraded database.
// The mirror of ErrDatabaseRolledBack, and just as unsafe to run.
type ErrDatabaseTooNew struct {
	DatabaseVersion int
	BinaryVersion   int
}

func (e *ErrDatabaseTooNew) Error() string {
	return fmt.Sprintf(
		"database schema version %d is newer than this binary's highest migration %d: "+
			"the database was upgraded by a later release. Deploy that release, or "+
			"restore a database matching this one",
		e.DatabaseVersion, e.BinaryVersion,
	)
}

// statePath returns the sidecar path for this database.
func (s *Store) statePath() string {
	return s.dbPath + dataDirStateSuffix
}

// MigrationState reports how many migrations the open database has applied
// and the highest version among them. A database with no schema_migrations
// table at all is brand new, reported as (0, 0).
func (s *Store) MigrationState(ctx context.Context) (count, maxVersion int, err error) {
	var maybeMax sql.NullInt64

	err = s.read.QueryRowContext(ctx,
		"SELECT COUNT(*), MAX(version) FROM schema_migrations",
	).Scan(&count, &maybeMax)

	if err != nil {
		// A fresh database has no schema_migrations table yet. Any other
		// failure is real and must surface.
		if strings.Contains(err.Error(), "no such table") {
			return 0, 0, nil
		}

		return 0, 0, fmt.Errorf("reading migration state: %w", err)
	}

	return count, int(maybeMax.Int64), nil
}

// highestEmbeddedMigration returns the newest migration version compiled
// into this binary.
func highestEmbeddedMigration() (int, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("reading migrations directory: %w", err)
	}

	highest := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, err := extractVersion(entry.Name())
		if err != nil {
			return 0, fmt.Errorf("parsing migration filename %s: %w", entry.Name(), err)
		}

		if version > highest {
			highest = version
		}
	}

	return highest, nil
}

// VerifyDataDirectory checks the open database against the sidecar marker
// left by previous runs, and against the migrations this binary carries.
//
// It catches the failure mode that silently destroyed a month of writes in
// production: a data directory whose uploads and sidecar had moved on,
// handed a database file rewound to an earlier state. Migrations happily
// replay on such a file, so nothing downstream notices.
//
// A missing sidecar is not an error — it is how every pre-existing install
// and every genuinely fresh database looks. The marker is established on
// the first run and enforced from the second onward.
//
// allowRollback downgrades a detected rollback to a loud warning, for the
// operator who really is restoring an older database on purpose.
func (s *Store) VerifyDataDirectory(ctx context.Context, allowRollback bool) error {
	appliedCount, dbVersion, err := s.MigrationState(ctx)
	if err != nil {
		return err
	}

	binaryVersion, err := highestEmbeddedMigration()
	if err != nil {
		return err
	}

	if dbVersion > binaryVersion {
		return &ErrDatabaseTooNew{DatabaseVersion: dbVersion, BinaryVersion: binaryVersion}
	}

	prev, err := s.readDataDirState()
	if err != nil {
		return err
	}

	if prev == nil {
		s.logger.InfoContext(ctx, "No data directory state file yet, establishing baseline",
			slog.String("state_file", s.statePath()),
			slog.Int("applied_migrations", appliedCount),
		)

		return nil
	}

	if appliedCount < prev.AppliedCount {
		rollback := &ErrDatabaseRolledBack{
			DatabaseAppliedCount: appliedCount,
			SidecarAppliedCount:  prev.AppliedCount,
			SidecarUpdatedAt:     prev.UpdatedAt,
			StatePath:            s.statePath(),
		}

		if !allowRollback {
			return rollback
		}

		s.logger.WarnContext(ctx,
			"Database rollback detected but ALLOW_DB_ROLLBACK is set, continuing",
			slog.String("detail", rollback.Error()),
		)
	}

	return nil
}

// RecordDataDirectoryState writes the sidecar marker. Call after migrations
// succeed, on every boot — including boots with nothing pending, so that an
// install predating this guard establishes its baseline.
func (s *Store) RecordDataDirectoryState(ctx context.Context) error {
	appliedCount, dbVersion, err := s.MigrationState(ctx)
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(dataDirState{
		SchemaVersion: dbVersion,
		AppliedCount:  appliedCount,
		UpdatedAt:     time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding data directory state: %w", err)
	}

	payload = append(payload, '\n')

	// Write-then-rename: a torn state file would fail the next boot closed
	// for no reason.
	dir := filepath.Dir(s.statePath())

	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}

	tmpName := tmp.Name()

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return fmt.Errorf("writing temp state file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return fmt.Errorf("syncing temp state file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("closing temp state file: %w", err)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("chmod temp state file: %w", err)
	}

	if err := os.Rename(tmpName, s.statePath()); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("installing state file: %w", err)
	}

	s.logger.InfoContext(ctx, "Recorded data directory state",
		slog.Int("applied_migrations", appliedCount),
		slog.Int("schema_version", dbVersion),
	)

	return nil
}

// readDataDirState loads the sidecar, returning nil when it does not exist.
func (s *Store) readDataDirState() (*dataDirState, error) {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading data directory state: %w", err)
	}

	var state dataDirState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parsing data directory state %s: %w", s.statePath(), err)
	}

	return &state, nil
}
