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

// dataDirStateSuffix names the sidecar file written next to the database on
// every boot. It records the database's high-water marks OUTSIDE the
// database, which is the whole point: a marker stored in the database
// itself is restored — and therefore rewound — along with it.
const dataDirStateSuffix = ".state"

// dataDirState is the sidecar payload.
//
// Two independent marks, because they catch different rollbacks:
//
//   - AppliedCount catches a database rewound across a schema upgrade (the
//     2026-08-05 incident: a pre-016 file handed to a post-021 data
//     directory).
//   - Generation catches a rewind that lands on the SAME schema, which the
//     count cannot see at all. It is a boot counter kept in the SQLite
//     header via PRAGMA user_version, so it rides along inside the file and
//     a stale snapshot carries a stale value.
//
// SchemaVersion and UpdatedAt exist to make the file readable by a human
// staring at a broken deploy at 2am.
type dataDirState struct {
	SchemaVersion int       `json:"schema_version"`
	AppliedCount  int       `json:"applied_count"`
	Generation    int       `json:"generation"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// rollbackKind names which high-water mark regressed, so the error message
// can say something specific.
type rollbackKind string

const (
	rollbackSchema     rollbackKind = "applied migrations"
	rollbackGeneration rollbackKind = "boot generation"
)

// ErrDatabaseRolledBack reports a database file older than the data
// directory it sits in — the fingerprint of a restore from a stale
// snapshot. Callers should refuse to start rather than migrate it forward:
// doing so replays the schema on top of old data and silently strands
// every row written since the snapshot.
type ErrDatabaseRolledBack struct {
	Kind             rollbackKind
	DatabaseValue    int
	SidecarValue     int
	SidecarUpdatedAt time.Time
	StatePath        string
}

func (e *ErrDatabaseRolledBack) Error() string {
	return fmt.Sprintf(
		"database reports %s = %d but this data directory last saw %d (%s): "+
			"the database file is older than the data directory, which usually means it "+
			"was restored from a stale snapshot or copied without its -wal sibling. "+
			"Continuing would strand every row written since then. "+
			"Restore the correct database, or set ALLOW_DB_ROLLBACK=true to proceed "+
			"deliberately (state file: %s)",
		e.Kind, e.DatabaseValue, e.SidecarValue,
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
		// failure is real and must surface. Matching on the message is
		// unpleasant but fails safe: if the driver ever rewords it, this
		// returns an error instead of pretending the database is empty.
		if strings.Contains(err.Error(), "no such table") {
			return 0, 0, nil
		}

		return 0, 0, fmt.Errorf("reading migration state: %w", err)
	}

	return count, int(maybeMax.Int64), nil
}

// DataGeneration reads the boot counter from the SQLite header. Zero means
// the counter has never been set — a fresh database, or one predating this
// guard.
func (s *Store) DataGeneration(ctx context.Context) (int, error) {
	var generation int

	if err := s.read.QueryRowContext(ctx,
		"PRAGMA user_version",
	).Scan(&generation); err != nil {
		return 0, fmt.Errorf("reading data generation: %w", err)
	}

	return generation, nil
}

// setDataGeneration writes the boot counter into the SQLite header. The
// pragma takes no bound parameters, so the value is formatted in; it is an
// int, never user input.
func (s *Store) setDataGeneration(ctx context.Context, generation int) error {
	if _, err := s.write.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", generation),
	); err != nil {
		return fmt.Errorf("setting data generation to %d: %w", generation, err)
	}

	return nil
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
// and every genuinely fresh database looks. The marks are established on
// the first run and enforced from the second onward. The sidecar must
// travel with the database: copy the data directory, not just the .db, or
// the guard re-baselines on the new host.
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

	generation, err := s.DataGeneration(ctx)
	if err != nil {
		return err
	}

	prev, err := s.readDataDirState()
	if err != nil {
		return err
	}

	if prev == nil {
		s.logger.InfoContext(ctx, "No data directory state file yet, establishing baseline",
			slog.String("state_file", s.statePath()),
			slog.Int("applied_migrations", appliedCount),
			slog.Int("generation", generation),
		)

		return nil
	}

	// Schema regression first: it names the more specific problem, and it
	// is the one an operator can act on by deploying a matching release.
	var rollback *ErrDatabaseRolledBack

	switch {
	case appliedCount < prev.AppliedCount:
		rollback = &ErrDatabaseRolledBack{
			Kind:          rollbackSchema,
			DatabaseValue: appliedCount,
			SidecarValue:  prev.AppliedCount,
		}
	case generation < prev.Generation:
		rollback = &ErrDatabaseRolledBack{
			Kind:          rollbackGeneration,
			DatabaseValue: generation,
			SidecarValue:  prev.Generation,
		}
	default:
		return nil
	}

	rollback.SidecarUpdatedAt = prev.UpdatedAt
	rollback.StatePath = s.statePath()

	if !allowRollback {
		return rollback
	}

	s.logger.WarnContext(ctx,
		"Database rollback detected but ALLOW_DB_ROLLBACK is set, continuing",
		slog.String("detail", rollback.Error()),
	)

	return nil
}

// RecordDataDirectoryState advances the boot generation and writes the
// sidecar. Call after migrations succeed, on every boot — including boots
// with nothing pending, so an install predating this guard establishes its
// baseline.
//
// The generation goes into the database first, then the sidecar. A crash
// between the two leaves the database ahead of the sidecar, which is not a
// rollback and so fails in the harmless direction.
func (s *Store) RecordDataDirectoryState(ctx context.Context) error {
	appliedCount, dbVersion, err := s.MigrationState(ctx)
	if err != nil {
		return err
	}

	dbGeneration, err := s.DataGeneration(ctx)
	if err != nil {
		return err
	}

	prev, err := s.readDataDirState()
	if err != nil {
		return err
	}

	// Monotonic across both marks: an allowed rollback must not be able to
	// hand the next boot a lower generation than the directory has seen.
	next := dbGeneration
	if prev != nil && prev.Generation > next {
		next = prev.Generation
	}

	next++

	if err := s.setDataGeneration(ctx, next); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(dataDirState{
		SchemaVersion: dbVersion,
		AppliedCount:  appliedCount,
		Generation:    next,
		UpdatedAt:     time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding data directory state: %w", err)
	}

	if err := s.writeStateFile(append(payload, '\n')); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "Recorded data directory state",
		slog.Int("applied_migrations", appliedCount),
		slog.Int("schema_version", dbVersion),
		slog.Int("generation", next),
	)

	return nil
}

// writeStateFile installs payload at statePath atomically and durably:
// write, fsync, rename, then fsync the directory so the rename itself
// survives a host crash. Without the directory sync a crash right after a
// migration can lose the new sidecar entirely, and the next boot would
// accept a stale database and re-baseline.
func (s *Store) writeStateFile(payload []byte) error {
	path := s.statePath()
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}

	tmpName := tmp.Name()

	// Every failure below removes the temp file; a stray .state-* would
	// otherwise accumulate in the database directory on each bad boot.
	cleanup := func(cause error) error {
		tmp.Close()
		os.Remove(tmpName)

		return cause
	}

	if _, err := tmp.Write(payload); err != nil {
		return cleanup(fmt.Errorf("writing temp state file: %w", err))
	}

	if err := tmp.Sync(); err != nil {
		return cleanup(fmt.Errorf("syncing temp state file: %w", err))
	}

	// Readable by anyone who can read the database beside it; CreateTemp
	// makes it 0600, and the deployment runs the app as a non-root uid.
	if err := tmp.Chmod(0o644); err != nil {
		return cleanup(fmt.Errorf("chmod temp state file: %w", err))
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("closing temp state file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("installing state file: %w", err)
	}

	return syncDir(dir)
}

// syncDir fsyncs a directory so a rename inside it is durable.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening state directory %s: %w", dir, err)
	}

	if err := handle.Sync(); err != nil {
		handle.Close()

		return fmt.Errorf("syncing state directory %s: %w", dir, err)
	}

	if err := handle.Close(); err != nil {
		return fmt.Errorf("closing state directory %s: %w", dir, err)
	}

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
