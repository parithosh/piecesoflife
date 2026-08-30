package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dataDirStateSuffix names the sidecar file written next to the database on
// every boot. It records the database's high-water mark OUTSIDE the
// database, which is the whole point: a marker stored in the database
// itself is restored — and therefore rewound — along with it.
const dataDirStateSuffix = ".state"

// dataDirState is the sidecar payload.
//
// Generation is the whole mechanism: a boot counter kept in the SQLite
// header via PRAGMA user_version, so it rides along inside the database
// file and a restored snapshot carries the value it had when it was taken.
// Anything older than the data directory it lands in therefore reports a
// lower number.
//
// SchemaVersion and AppliedCount are context for a human staring at a
// broken deploy at 2am. They are not compared: any snapshot old enough to
// have fewer migrations is also old enough to have a lower generation.
type dataDirState struct {
	Generation    int       `json:"generation"`
	SchemaVersion int       `json:"schema_version"`
	AppliedCount  int       `json:"applied_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// statePath returns the sidecar path for this database.
func (s *Store) statePath() string {
	return s.dbPath + dataDirStateSuffix
}

// migrationState reports how many migrations the open database has applied
// and the highest version among them, for the sidecar's benefit. A database
// with no schema_migrations table at all is brand new, reported as (0, 0).
func (s *Store) migrationState(ctx context.Context) (count, maxVersion int, err error) {
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

// VerifyDataDirectory refuses to continue when the open database is older
// than the data directory it sits in.
//
// That is the failure mode which silently destroyed a month of writes in
// production: a data directory whose uploads had moved on, handed a
// database file rewound to an earlier state. Migrations happily replay on
// such a file, so nothing downstream notices.
//
// A missing sidecar is not an error — it is how every pre-existing install
// and every genuinely fresh database looks. The mark is established on the
// first run and enforced from the second onward. The sidecar must travel
// with the database: copy the data directory, not just the .db, or the
// guard re-baselines on the new host.
//
// Deleting the sidecar is the escape hatch for an operator who really is
// restoring an older database. It is deliberately one-shot: unlike an
// environment variable it cannot be left switched on, and the next boot
// re-arms the guard.
func (s *Store) VerifyDataDirectory(ctx context.Context) error {
	prev, err := s.readDataDirState()
	if err != nil {
		return err
	}

	generation, err := s.DataGeneration(ctx)
	if err != nil {
		return err
	}

	if prev == nil {
		s.logger.InfoContext(ctx, "No data directory state file yet, establishing baseline",
			slog.String("state_file", s.statePath()),
			slog.Int("generation", generation),
		)

		return nil
	}

	if generation >= prev.Generation {
		return nil
	}

	return fmt.Errorf(
		"database is at boot generation %d but this data directory last saw %d (%s): "+
			"the database file is older than the data directory, which usually means it "+
			"was restored from a stale snapshot or copied without its -wal sibling. "+
			"Continuing would strand every row written since then. Restore the correct "+
			"database, or delete %s to accept this one",
		generation, prev.Generation,
		prev.UpdatedAt.UTC().Format(time.RFC3339), s.statePath(),
	)
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
	appliedCount, dbVersion, err := s.migrationState(ctx)
	if err != nil {
		return err
	}

	generation, err := s.DataGeneration(ctx)
	if err != nil {
		return err
	}

	generation++

	if err := s.setDataGeneration(ctx, generation); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(dataDirState{
		Generation:    generation,
		SchemaVersion: dbVersion,
		AppliedCount:  appliedCount,
		UpdatedAt:     time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding data directory state: %w", err)
	}

	if err := s.writeStateFile(append(payload, '\n')); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "Recorded data directory state",
		slog.Int("generation", generation),
		slog.Int("applied_migrations", appliedCount),
		slog.Int("schema_version", dbVersion),
	)

	return nil
}

// writeStateFile installs payload at statePath by write-sync-rename, so a
// crash can never leave a half-written sidecar that fails to parse and
// blocks the next boot for no reason.
func (s *Store) writeStateFile(payload []byte) error {
	path := s.statePath()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
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
