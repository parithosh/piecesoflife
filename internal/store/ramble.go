package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Ramble is one page of a member's private journal: one row per person per
// calendar day. Person-scoped — there is deliberately no group_id; the
// journal spans every Loop the member belongs to. `Day` is a local
// 'YYYY-MM-DD' label chosen by the client when the page was opened, not a
// timestamp.
type Ramble struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Day       string    `json:"day"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RambleBlock is an ordered content block within a ramble day — the same
// shape as a response block, minus links.
type RambleBlock struct {
	ID        int64     `json:"id"`
	RambleID  int64     `json:"ramble_id"`
	Type      string    `json:"type"`
	Content   *string   `json:"content"`
	FilePath  *string   `json:"file_path"`
	Caption   *string   `json:"caption"`
	SortOrder int       `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RambleDay bundles a journal day with its blocks.
type RambleDay struct {
	Ramble Ramble        `json:"ramble"`
	Blocks []RambleBlock `json:"blocks"`
}

// GetRambleByDay returns a user's journal page for one day.
func (s *Store) GetRambleByDay(
	ctx context.Context, userID int64, day string,
) (*Ramble, error) {
	var r Ramble

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, day, created_at, updated_at
		 FROM rambles WHERE user_id = ? AND day = ?`, userID, day,
	).Scan(&r.ID, &r.UserID, &r.Day, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting ramble for day %s: %w", day, err)
	}

	return &r, nil
}

// GetRambleByID returns a journal page by its database ID.
func (s *Store) GetRambleByID(ctx context.Context, id int64) (*Ramble, error) {
	var r Ramble

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, day, created_at, updated_at
		 FROM rambles WHERE id = ?`, id,
	).Scan(&r.ID, &r.UserID, &r.Day, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting ramble %d: %w", id, err)
	}

	return &r, nil
}

// ListRambleDays returns every journal day for a user, newest first, each
// with its blocks in order.
func (s *Store) ListRambleDays(
	ctx context.Context, userID int64,
) ([]RambleDay, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, user_id, day, created_at, updated_at
		 FROM rambles WHERE user_id = ?
		 ORDER BY day DESC`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing rambles for user %d: %w", userID, err)
	}
	defer rows.Close()

	days := make([]RambleDay, 0, 32)
	index := make(map[int64]int, 32)

	for rows.Next() {
		var r Ramble
		if err := rows.Scan(&r.ID, &r.UserID, &r.Day,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning ramble: %w", err)
		}

		index[r.ID] = len(days)
		days = append(days, RambleDay{Ramble: r, Blocks: make([]RambleBlock, 0, 4)})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rambles: %w", err)
	}

	blockRows, err := s.read.QueryContext(ctx,
		`SELECT b.id, b.ramble_id, b.type, b.content, b.file_path, b.caption,
		        b.sort_order, b.created_at, b.updated_at
		 FROM ramble_blocks b
		 JOIN rambles r ON r.id = b.ramble_id
		 WHERE r.user_id = ?
		 ORDER BY b.ramble_id, b.sort_order`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing ramble blocks for user %d: %w", userID, err)
	}
	defer blockRows.Close()

	for blockRows.Next() {
		var b RambleBlock
		if err := blockRows.Scan(&b.ID, &b.RambleID, &b.Type, &b.Content,
			&b.FilePath, &b.Caption, &b.SortOrder,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning ramble block: %w", err)
		}

		if i, ok := index[b.RambleID]; ok {
			days[i].Blocks = append(days[i].Blocks, b)
		}
	}

	if err := blockRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ramble blocks: %w", err)
	}

	return days, nil
}

// ListRambleBlocks returns a journal day's blocks in order.
func (s *Store) ListRambleBlocks(
	ctx context.Context, rambleID int64,
) ([]RambleBlock, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, ramble_id, type, content, file_path, caption,
		        sort_order, created_at, updated_at
		 FROM ramble_blocks WHERE ramble_id = ?
		 ORDER BY sort_order`, rambleID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing blocks for ramble %d: %w", rambleID, err)
	}
	defer rows.Close()

	blocks := make([]RambleBlock, 0, 4)

	for rows.Next() {
		var b RambleBlock
		if err := rows.Scan(&b.ID, &b.RambleID, &b.Type, &b.Content,
			&b.FilePath, &b.Caption, &b.SortOrder,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning ramble block: %w", err)
		}

		blocks = append(blocks, b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ramble blocks: %w", err)
	}

	return blocks, nil
}

// AutosaveRambleDay replaces the text blocks of a user's journal page for
// one day, creating the page if needed. Media blocks are managed through
// their own endpoints and survive untouched — same contract as response
// autosave. A page left with no blocks at all is deleted (empty days are
// invisible), reported via removed=true.
func (s *Store) AutosaveRambleDay(
	ctx context.Context, userID int64, day string, blocks []RambleBlock,
) (rambleID int64, removed bool, err error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("beginning ramble autosave: %w", err)
	}

	defer tx.Rollback()

	err = tx.QueryRowContext(ctx,
		"SELECT id FROM rambles WHERE user_id = ? AND day = ?", userID, day,
	).Scan(&rambleID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, insErr := tx.ExecContext(ctx,
			"INSERT INTO rambles (user_id, day) VALUES (?, ?)", userID, day)
		if insErr != nil {
			return 0, false, fmt.Errorf("creating ramble day: %w", insErr)
		}

		rambleID, insErr = res.LastInsertId()
		if insErr != nil {
			return 0, false, fmt.Errorf("getting new ramble id: %w", insErr)
		}
	case err != nil:
		return 0, false, fmt.Errorf("finding ramble day: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM ramble_blocks WHERE ramble_id = ? AND type = 'text'",
		rambleID,
	); err != nil {
		return 0, false, fmt.Errorf("deleting existing text blocks: %w", err)
	}

	var maxSort sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT MAX(sort_order) FROM ramble_blocks WHERE ramble_id = ?", rambleID,
	).Scan(&maxSort); err != nil {
		return 0, false, fmt.Errorf("finding max sort order: %w", err)
	}

	nextSort := int64(-1)
	if maxSort.Valid {
		nextSort = maxSort.Int64
	}

	inserted := 0

	for i, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			continue // defence in depth — the handler already rejects these
		}
		if b.Content == nil || *b.Content == "" {
			continue // empty text is absence, not a block
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ramble_blocks (ramble_id, type, content, sort_order)
			 VALUES (?, 'text', ?, ?)`,
			rambleID, b.Content, nextSort+1+int64(i),
		); err != nil {
			return 0, false, fmt.Errorf("inserting text block %d: %w", i, err)
		}

		inserted++
	}

	var remaining int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM ramble_blocks WHERE ramble_id = ?", rambleID,
	).Scan(&remaining); err != nil {
		return 0, false, fmt.Errorf("counting remaining blocks: %w", err)
	}

	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM rambles WHERE id = ?", rambleID); err != nil {
			return 0, false, fmt.Errorf("removing empty ramble day: %w", err)
		}

		removed = true
	} else {
		if _, err := tx.ExecContext(ctx,
			"UPDATE rambles SET updated_at = CURRENT_TIMESTAMP WHERE id = ?",
			rambleID); err != nil {
			return 0, false, fmt.Errorf("touching ramble day: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("committing ramble autosave: %w", err)
	}

	return rambleID, removed, nil
}

// EnsureRambleDay returns the id of the user's page for a day, creating an
// empty page when none exists — media uploads need a row to attach to
// before any text has been written.
func (s *Store) EnsureRambleDay(
	ctx context.Context, userID int64, day string,
) (int64, error) {
	var id int64

	err := s.read.QueryRowContext(ctx,
		"SELECT id FROM rambles WHERE user_id = ? AND day = ?", userID, day,
	).Scan(&id)
	if err == nil {
		return id, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("finding ramble day: %w", err)
	}

	res, err := s.write.ExecContext(ctx,
		"INSERT INTO rambles (user_id, day) VALUES (?, ?)", userID, day)
	if err != nil {
		return 0, fmt.Errorf("creating ramble day: %w", err)
	}

	id, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new ramble id: %w", err)
	}

	return id, nil
}

// CreateRambleBlock inserts a media block on a journal day, taking the next
// sort slot.
func (s *Store) CreateRambleBlock(
	ctx context.Context,
	rambleID int64, blockType string,
	content, filePath, caption *string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO ramble_blocks
		     (ramble_id, type, content, file_path, caption, sort_order)
		 VALUES (?, ?, ?, ?, ?,
		     (SELECT COALESCE(MAX(sort_order) + 1, 0) FROM ramble_blocks
		      WHERE ramble_id = ?))`,
		rambleID, blockType, content, filePath, caption, rambleID,
	)
	if err != nil {
		return 0, fmt.Errorf("creating ramble block: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new ramble block id: %w", err)
	}

	return id, nil
}

// GetRambleBlockByID returns a journal block by its database ID.
func (s *Store) GetRambleBlockByID(
	ctx context.Context, id int64,
) (*RambleBlock, error) {
	var b RambleBlock

	err := s.read.QueryRowContext(ctx,
		`SELECT id, ramble_id, type, content, file_path, caption,
		        sort_order, created_at, updated_at
		 FROM ramble_blocks WHERE id = ?`, id,
	).Scan(&b.ID, &b.RambleID, &b.Type, &b.Content, &b.FilePath,
		&b.Caption, &b.SortOrder, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting ramble block %d: %w", id, err)
	}

	return &b, nil
}

// DeleteRambleBlock removes a journal block; a day left with no blocks is
// deleted with it (empty days are invisible).
func (s *Store) DeleteRambleBlock(ctx context.Context, id int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning ramble block delete: %w", err)
	}

	defer tx.Rollback()

	var rambleID int64
	if err := tx.QueryRowContext(ctx,
		"SELECT ramble_id FROM ramble_blocks WHERE id = ?", id,
	).Scan(&rambleID); err != nil {
		return fmt.Errorf("finding ramble block %d: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM ramble_blocks WHERE id = ?", id); err != nil {
		return fmt.Errorf("deleting ramble block %d: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rambles WHERE id = ?
		 AND NOT EXISTS (SELECT 1 FROM ramble_blocks WHERE ramble_id = ?)`,
		rambleID, rambleID); err != nil {
		return fmt.Errorf("removing empty ramble day: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing ramble block delete: %w", err)
	}

	return nil
}

// CountRambleBlocksForType counts a day's blocks of one type — enforces the
// per-day media caps.
func (s *Store) CountRambleBlocksForType(
	ctx context.Context, rambleID int64, blockType string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ramble_blocks
		 WHERE ramble_id = ? AND type = ?`, rambleID, blockType,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting %s ramble blocks: %w", blockType, err)
	}

	return count, nil
}

// CountRambleDaysBetween counts a user's journal days in [fromDay,
// throughDay], inclusive on both ends. Day strings compare lexicographically.
// An empty fromDay means "from the beginning".
func (s *Store) CountRambleDaysBetween(
	ctx context.Context, userID int64, fromDay, throughDay string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rambles
		 WHERE user_id = ? AND day >= ? AND day <= ?`,
		userID, fromDay, throughDay,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting ramble days: %w", err)
	}

	return count, nil
}

// CountPullableRambleDays counts journal days a diary refresh would copy:
// days from the section's copied_through (inclusive, matching
// RefreshDiarySection) up to throughDay that hold content and aren't in the
// section yet — the "pull in N new days" affordance.
func (s *Store) CountPullableRambleDays(
	ctx context.Context, userID, sectionID int64, throughDay string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rambles r
		 WHERE r.user_id = ?
		   AND r.day >= (SELECT copied_through FROM diary_sections WHERE id = ?)
		   AND r.day <= ?
		   AND EXISTS (SELECT 1 FROM ramble_blocks rb WHERE rb.ramble_id = r.id)
		   AND NOT EXISTS (SELECT 1 FROM diary_days dd
		                   WHERE dd.section_id = ? AND dd.day = r.day)`,
		userID, sectionID, throughDay, sectionID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting pullable ramble days: %w", err)
	}

	return count, nil
}

// CountUploadsReferencing reports how many journal and diary blocks point at
// an uploaded file. Ramble uploads are shared by path with their diary
// snapshot copies, so deletion paths must check this before unlinking.
func (s *Store) CountUploadsReferencing(
	ctx context.Context, filePath string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT
		    (SELECT COUNT(*) FROM ramble_blocks WHERE file_path = ?) +
		    (SELECT COUNT(*) FROM diary_blocks WHERE file_path = ?)`,
		filePath, filePath,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting upload references: %w", err)
	}

	return count, nil
}
