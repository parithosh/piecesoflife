// Package store provides database access, domain types, and query functions.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// questionSeedData is loaded from the seed directory embedded alongside the store package.
//
//go:embed seed/questions.json
var questionSeedData []byte

func init() {
	sqlite.RegisterConnectionHook(func(
		conn sqlite.ExecQuerierContext, dsn string,
	) error {
		pragmas := []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA foreign_keys=ON",
			"PRAGMA busy_timeout=5000",
			"PRAGMA synchronous=NORMAL",
		}

		for _, p := range pragmas {
			if _, err := conn.ExecContext(context.Background(), p, nil); err != nil {
				return fmt.Errorf("executing %s: %w", p, err)
			}
		}

		return nil
	})
}

// Store provides database access with separate read and write connection pools.
type Store struct {
	write  *sql.DB
	read   *sql.DB
	logger *slog.Logger
}

// New opens the SQLite database at the given path and returns a Store
// with separate read and write connection pools.
func New(ctx context.Context, dbPath string, logger *slog.Logger) (*Store, error) {
	log := logger.With(slog.String("component", "store"))

	writeDSN := fmt.Sprintf("file:%s?cache=shared&_txlock=immediate", dbPath)
	readDSN := fmt.Sprintf("file:%s?cache=shared", dbPath)

	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("opening write db: %w", err)
	}

	writeDB.SetMaxOpenConns(1)

	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("opening read db: %w", err)
	}

	readDB.SetMaxOpenConns(4)

	if err := writeDB.PingContext(ctx); err != nil {
		writeDB.Close()
		readDB.Close()

		return nil, fmt.Errorf("pinging write db: %w", err)
	}

	if err := readDB.PingContext(ctx); err != nil {
		writeDB.Close()
		readDB.Close()

		return nil, fmt.Errorf("pinging read db: %w", err)
	}

	log.InfoContext(ctx, "Database connection established", slog.String("path", dbPath))

	return &Store{
		write:  writeDB,
		read:   readDB,
		logger: log,
	}, nil
}

// Close closes both read and write database connections.
func (s *Store) Close() error {
	var errs []error

	if err := s.write.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing write db: %w", err))
	}

	if err := s.read.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing read db: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("closing store: %v", errs)
	}

	return nil
}

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.read.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	return nil
}

// RunMigrations applies any pending SQL migration files.
func (s *Store) RunMigrations(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading migrations directory: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, err := extractVersion(entry.Name())
		if err != nil {
			return fmt.Errorf("parsing migration filename %s: %w", entry.Name(), err)
		}

		var applied int
		err = s.write.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version = ?",
			version,
		).Scan(&applied)
		if err != nil {
			return fmt.Errorf("checking migration %d: %w", version, err)
		}

		if applied > 0 {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", entry.Name(), err)
		}

		if err := s.applyMigration(ctx, entry.Name(), version, string(sqlBytes)); err != nil {
			return err
		}

		s.logger.InfoContext(ctx, "Applied migration",
			slog.Int("version", version),
			slog.String("file", entry.Name()),
		)
	}

	return nil
}

// applyMigration runs one migration inside a transaction.
//
// Migrations containing the "migrate:fk_off" marker rebuild a table that
// other tables reference — SQLite's documented procedure for that requires
// foreign_keys OFF for the duration (the pragma is a no-op inside a
// transaction, and the write pool is capped at one connection, so toggling
// it on s.write is guaranteed to affect the transaction's connection).
// After the rebuild, foreign_key_check verifies nothing dangles before the
// commit, and the pragma is always restored.
func (s *Store) applyMigration(
	ctx context.Context, name string, version int, sqlText string,
) error {
	fkOff := strings.Contains(sqlText, "migrate:fk_off")

	if fkOff {
		if _, err := s.write.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			return fmt.Errorf("disabling foreign keys for migration %s: %w", name, err)
		}

		defer func() {
			if _, err := s.write.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
				s.logger.ErrorContext(ctx, "Failed to re-enable foreign keys after migration",
					slog.String("file", name),
					slog.String("error", err.Error()))
			}
		}()
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning migration transaction: %w", err)
	}

	if _, err := tx.ExecContext(ctx, sqlText); err != nil {
		tx.Rollback()
		return fmt.Errorf("executing migration %s: %w", name, err)
	}

	if fkOff {
		// The rebuild ran unchecked — make sure no references dangle
		// before this becomes permanent.
		var violations int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM pragma_foreign_key_check",
		).Scan(&violations); err != nil {
			tx.Rollback()
			return fmt.Errorf("verifying foreign keys after migration %s: %w", name, err)
		}
		if violations > 0 {
			tx.Rollback()
			return fmt.Errorf("migration %s left %d dangling foreign key reference(s)", name, violations)
		}
	}

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version) VALUES (?)", version,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("recording migration %d: %w", version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %d: %w", version, err)
	}

	return nil
}

// SeedAdminUser ensures the ADMIN_EMAIL operator exists as an instance
// admin with an admin membership in the default (oldest active) group.
// No-op once any instance admin exists.
func (s *Store) SeedAdminUser(ctx context.Context, email string) error {
	var count int

	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM users WHERE is_instance_admin = 1",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking for existing instance admin: %w", err)
	}

	if count > 0 {
		return nil
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning seed transaction: %w", err)
	}

	defer tx.Rollback()

	var groupID int64
	if err := tx.QueryRowContext(ctx,
		"SELECT id FROM groups WHERE is_active = 1 ORDER BY created_at, id LIMIT 1",
	).Scan(&groupID); err != nil {
		return fmt.Errorf("finding default group for admin seed: %w", err)
	}

	var userID int64

	err = tx.QueryRowContext(ctx,
		"SELECT id FROM users WHERE email = ?", email,
	).Scan(&userID)

	switch {
	case err == sql.ErrNoRows:
		result, err := tx.ExecContext(ctx,
			`INSERT INTO users (name, email, is_active, is_instance_admin, last_group_id)
			 VALUES ('Admin', ?, 1, 1, ?)`,
			email, groupID,
		)
		if err != nil {
			return fmt.Errorf("inserting admin user: %w", err)
		}

		userID, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("getting admin user id: %w", err)
		}
	case err != nil:
		return fmt.Errorf("finding admin user: %w", err)
	default:
		if _, err := tx.ExecContext(ctx,
			"UPDATE users SET is_instance_admin = 1 WHERE id = ?", userID,
		); err != nil {
			return fmt.Errorf("promoting admin user: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memberships (group_id, user_id, role, is_active)
		 VALUES (?, ?, 'admin', 1)
		 ON CONFLICT(group_id, user_id) DO UPDATE SET role = 'admin', is_active = 1`,
		groupID, userID,
	); err != nil {
		return fmt.Errorf("inserting admin membership: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO notification_preferences (user_id) VALUES (?)",
		userID,
	)
	if err != nil {
		return fmt.Errorf("inserting admin notification preferences: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing admin seed: %w", err)
	}

	s.logger.InfoContext(ctx, "Seeded instance admin", slog.String("email", email))

	return nil
}

// SeedDefaultGroup creates the first Loop (with settings, default
// questions, and question bank) on a fresh database. No-op once any group
// exists, so migrated installs keep their group 1 untouched.
func (s *Store) SeedDefaultGroup(ctx context.Context) error {
	var count int

	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM groups",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking for existing groups: %w", err)
	}

	if count > 0 {
		return nil
	}

	groupID, err := s.CreateGroup(ctx, "PiecesOfLife")
	if err != nil {
		return fmt.Errorf("seeding default group: %w", err)
	}

	s.logger.InfoContext(ctx, "Seeded default group", slog.Int64("group_id", groupID))

	return nil
}

// SeedQuestionBank tops up every group's bank from the embedded seed file.
// Runs on every startup: (group_id, text) is UNIQUE and the insert is OR
// IGNORE, so existing rows (and their used flags) are untouched while
// questions added to the seed file in later releases flow into existing
// Loops.
func (s *Store) SeedQuestionBank(ctx context.Context) error {
	rows, err := s.read.QueryContext(ctx, "SELECT id FROM groups")
	if err != nil {
		return fmt.Errorf("listing groups for question seed: %w", err)
	}
	defer rows.Close()

	groupIDs := make([]int64, 0, 4)

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning group id for question seed: %w", err)
		}

		groupIDs = append(groupIDs, id)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating groups for question seed: %w", err)
	}

	for _, groupID := range groupIDs {
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("beginning question seed transaction: %w", err)
		}

		if err := seedQuestionBankTx(ctx, tx, groupID); err != nil {
			tx.Rollback()
			return fmt.Errorf("seeding question bank for group %d: %w", groupID, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing question seed for group %d: %w", groupID, err)
		}
	}

	return nil
}

// seedQuestionBankTx inserts the embedded seed questions for one group
// inside an existing transaction, skipping texts the group already has.
func seedQuestionBankTx(ctx context.Context, tx *sql.Tx, groupID int64) error {
	var questions []struct {
		Text     string `json:"text"`
		Category string `json:"category"`
	}

	if err := json.Unmarshal(questionSeedData, &questions); err != nil {
		return fmt.Errorf("parsing question seed data: %w", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR IGNORE INTO question_bank (group_id, text, category) VALUES (?, ?, ?)",
	)
	if err != nil {
		return fmt.Errorf("preparing question insert: %w", err)
	}

	defer stmt.Close()

	for _, q := range questions {
		if _, err := stmt.ExecContext(ctx, groupID, q.Text, q.Category); err != nil {
			return fmt.Errorf("inserting question %q: %w", q.Text, err)
		}
	}

	return nil
}

func extractVersion(filename string) (int, error) {
	parts := strings.SplitN(filename, "_", 2)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid migration filename: %s", filename)
	}

	return strconv.Atoi(parts[0])
}
