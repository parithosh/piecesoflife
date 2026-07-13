package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDiaryAttached indicates the member already has a diary section on this
// issue.
var ErrDiaryAttached = errors.New("diary section already attached")

// DiarySection is one member's "From the notebooks" spread for one issue: an
// explicit, editable SNAPSHOT of their journal days. Editing a section never
// touches the journal, and refresh only offers days after CopiedThrough — a
// day the member trimmed out of the review never reappears.
type DiarySection struct {
	ID            int64     `json:"id"`
	IssueID       int64     `json:"issue_id"`
	UserID        int64     `json:"user_id"`
	CopiedThrough string    `json:"copied_through"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// DiaryDay is one snapshot day within a section.
type DiaryDay struct {
	ID        int64     `json:"id"`
	SectionID int64     `json:"section_id"`
	Day       string    `json:"day"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DiaryBlock is an ordered content block within a snapshot day.
type DiaryBlock struct {
	ID         int64     `json:"id"`
	DiaryDayID int64     `json:"diary_day_id"`
	Type       string    `json:"type"`
	Content    *string   `json:"content"`
	FilePath   *string   `json:"file_path"`
	Caption    *string   `json:"caption"`
	SortOrder  int       `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// DiaryDayWithBlocks bundles a snapshot day with its blocks.
type DiaryDayWithBlocks struct {
	DiaryDay DiaryDay     `json:"day"`
	Blocks   []DiaryBlock `json:"blocks"`
}

// DiarySectionWithDays is a member's full section — the respond page's
// review UI and the export both consume this.
type DiarySectionWithDays struct {
	Section DiarySection         `json:"section"`
	Days    []DiaryDayWithBlocks `json:"days"`
}

// DiaryGroup is a section joined with its owner for the published issue's
// notebook spread.
type DiaryGroup struct {
	Section       DiarySection         `json:"section"`
	UserID        int64                `json:"user_id"`
	UserName      string               `json:"user_name"`
	UserAvatarURL *string              `json:"user_avatar_url"`
	Days          []DiaryDayWithBlocks `json:"days"`
}

// AttachDiarySection snapshots a member's journal days in [fromDay,
// throughDay] into a new section on an issue. Returns the section id and how
// many days were copied. ErrDiaryAttached when a section already exists.
func (s *Store) AttachDiarySection(
	ctx context.Context,
	issueID, userID int64, fromDay, throughDay string,
) (int64, int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("beginning diary attach: %w", err)
	}

	defer tx.Rollback()

	var existing int64

	err = tx.QueryRowContext(ctx,
		"SELECT id FROM diary_sections WHERE issue_id = ? AND user_id = ?",
		issueID, userID,
	).Scan(&existing)

	switch {
	case err == nil:
		return 0, 0, ErrDiaryAttached
	case !errors.Is(err, sql.ErrNoRows):
		return 0, 0, fmt.Errorf("checking existing diary section: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO diary_sections (issue_id, user_id, copied_through)
		 VALUES (?, ?, ?)`,
		issueID, userID, throughDay,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("creating diary section: %w", err)
	}

	sectionID, err := res.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("getting new diary section id: %w", err)
	}

	copied, err := copyDiaryDays(ctx, tx, sectionID, userID, fromDay, throughDay)
	if err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("committing diary attach: %w", err)
	}

	return sectionID, copied, nil
}

// RefreshDiarySection copies journal days written since the section's
// copied_through (up to throughDay) into the section, then advances
// copied_through. Returns how many days were added.
//
// The lower bound is INCLUSIVE: a page that grows later on the same day the
// section was attached must stay pullable. The dedupe in copyDiaryDays
// skips days still present in the section; the one consequence is that a
// boundary day the member trimmed can be re-offered — visible and editable
// in the review either way, so nothing publishes unseen.
func (s *Store) RefreshDiarySection(
	ctx context.Context, sectionID, userID int64, throughDay string,
) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning diary refresh: %w", err)
	}

	defer tx.Rollback()

	var copiedThrough string
	if err := tx.QueryRowContext(ctx,
		"SELECT copied_through FROM diary_sections WHERE id = ?", sectionID,
	).Scan(&copiedThrough); err != nil {
		return 0, fmt.Errorf("loading diary section %d: %w", sectionID, err)
	}

	if throughDay < copiedThrough {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("committing no-op diary refresh: %w", err)
		}

		return 0, nil
	}

	added, err := copyDiaryDays(ctx, tx, sectionID, userID,
		copiedThrough, throughDay)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE diary_sections SET copied_through = ?,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		throughDay, sectionID,
	); err != nil {
		return 0, fmt.Errorf("advancing copied_through: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing diary refresh: %w", err)
	}

	return added, nil
}

// GetDiarySection returns a member's section on an issue.
func (s *Store) GetDiarySection(
	ctx context.Context, issueID, userID int64,
) (*DiarySection, error) {
	return s.scanDiarySection(s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, user_id, copied_through, created_at, updated_at
		 FROM diary_sections WHERE issue_id = ? AND user_id = ?`,
		issueID, userID,
	))
}

// GetDiarySectionByID returns a section by its database ID.
func (s *Store) GetDiarySectionByID(
	ctx context.Context, id int64,
) (*DiarySection, error) {
	return s.scanDiarySection(s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, user_id, copied_through, created_at, updated_at
		 FROM diary_sections WHERE id = ?`, id,
	))
}

// GetDiaryDayByID returns a snapshot day by its database ID.
func (s *Store) GetDiaryDayByID(
	ctx context.Context, id int64,
) (*DiaryDay, error) {
	var d DiaryDay

	err := s.read.QueryRowContext(ctx,
		`SELECT id, section_id, day, created_at, updated_at
		 FROM diary_days WHERE id = ?`, id,
	).Scan(&d.ID, &d.SectionID, &d.Day, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting diary day %d: %w", id, err)
	}

	return &d, nil
}

// GetDiaryBlockByID returns a snapshot block by its database ID.
func (s *Store) GetDiaryBlockByID(
	ctx context.Context, id int64,
) (*DiaryBlock, error) {
	var b DiaryBlock

	err := s.read.QueryRowContext(ctx,
		`SELECT id, diary_day_id, type, content, file_path, caption,
		        sort_order, created_at, updated_at
		 FROM diary_blocks WHERE id = ?`, id,
	).Scan(&b.ID, &b.DiaryDayID, &b.Type, &b.Content, &b.FilePath,
		&b.Caption, &b.SortOrder, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting diary block %d: %w", id, err)
	}

	return &b, nil
}

// GetIssueByDiaryDayID resolves the issue that owns a snapshot day — the
// guard path for day-level endpoints (comments, edits).
func (s *Store) GetIssueByDiaryDayID(
	ctx context.Context, dayID int64,
) (*Issue, error) {
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT i.id, i.group_id, i.title, i.month, i.year, i.status,
		        i.opens_at, i.deadline, i.published_at, i.created_at
		 FROM issues i
		 JOIN diary_sections ds ON ds.issue_id = i.id
		 JOIN diary_days dd ON dd.section_id = ds.id
		 WHERE dd.id = ?`, dayID,
	))
	if err != nil {
		return nil, fmt.Errorf("getting issue for diary day %d: %w", dayID, err)
	}

	return iss, nil
}

// ListDiaryDays returns a section's days oldest-first (reading order), each
// with its blocks.
func (s *Store) ListDiaryDays(
	ctx context.Context, sectionID int64,
) ([]DiaryDayWithBlocks, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, section_id, day, created_at, updated_at
		 FROM diary_days WHERE section_id = ?
		 ORDER BY day`, sectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing diary days for section %d: %w", sectionID, err)
	}
	defer rows.Close()

	days := make([]DiaryDayWithBlocks, 0, 16)
	index := make(map[int64]int, 16)

	for rows.Next() {
		var d DiaryDay
		if err := rows.Scan(&d.ID, &d.SectionID, &d.Day,
			&d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning diary day: %w", err)
		}

		index[d.ID] = len(days)
		days = append(days, DiaryDayWithBlocks{
			DiaryDay: d,
			Blocks:   make([]DiaryBlock, 0, 4),
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating diary days: %w", err)
	}

	blockRows, err := s.read.QueryContext(ctx,
		`SELECT b.id, b.diary_day_id, b.type, b.content, b.file_path,
		        b.caption, b.sort_order, b.created_at, b.updated_at
		 FROM diary_blocks b
		 JOIN diary_days d ON d.id = b.diary_day_id
		 WHERE d.section_id = ?
		 ORDER BY b.diary_day_id, b.sort_order`, sectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing diary blocks for section %d: %w", sectionID, err)
	}
	defer blockRows.Close()

	for blockRows.Next() {
		var b DiaryBlock
		if err := blockRows.Scan(&b.ID, &b.DiaryDayID, &b.Type, &b.Content,
			&b.FilePath, &b.Caption, &b.SortOrder,
			&b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scanning diary block: %w", err)
		}

		if i, ok := index[b.DiaryDayID]; ok {
			days[i].Blocks = append(days[i].Blocks, b)
		}
	}

	if err := blockRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating diary blocks: %w", err)
	}

	return days, nil
}

// ListDiarySectionsByIssue returns every section on an issue joined with its
// owner, days included, ordered by owner name — the notebook spread's render
// order.
func (s *Store) ListDiarySectionsByIssue(
	ctx context.Context, issueID int64,
) ([]DiaryGroup, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT ds.id, ds.issue_id, ds.user_id, ds.copied_through,
		        ds.created_at, ds.updated_at, u.name, u.avatar_url
		 FROM diary_sections ds
		 JOIN users u ON u.id = ds.user_id
		 WHERE ds.issue_id = ?
		 ORDER BY u.name COLLATE NOCASE`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing diary sections for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	groups := make([]DiaryGroup, 0, 8)

	for rows.Next() {
		var g DiaryGroup
		if err := rows.Scan(&g.Section.ID, &g.Section.IssueID,
			&g.Section.UserID, &g.Section.CopiedThrough,
			&g.Section.CreatedAt, &g.Section.UpdatedAt,
			&g.UserName, &g.UserAvatarURL); err != nil {
			return nil, fmt.Errorf("scanning diary section: %w", err)
		}

		g.UserID = g.Section.UserID
		groups = append(groups, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating diary sections: %w", err)
	}

	for i := range groups {
		days, err := s.ListDiaryDays(ctx, groups[i].Section.ID)
		if err != nil {
			return nil, err
		}

		// A section whose every day was trimmed to nothing renders as no
		// spread at all — keep it out of the groups.
		groups[i].Days = days
	}

	kept := groups[:0]
	for _, g := range groups {
		if len(g.Days) > 0 {
			kept = append(kept, g)
		}
	}

	return kept, nil
}

// AutosaveDiaryDay replaces a snapshot day's text blocks. Media blocks are
// managed via their own delete endpoint and survive untouched. A day left
// with no blocks is deleted (removed=true) — trimming everything out of a
// day removes it from the spread.
func (s *Store) AutosaveDiaryDay(
	ctx context.Context, dayID int64, blocks []DiaryBlock,
) (removed bool, err error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning diary day autosave: %w", err)
	}

	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM diary_blocks WHERE diary_day_id = ? AND type = 'text'",
		dayID,
	); err != nil {
		return false, fmt.Errorf("deleting existing text blocks: %w", err)
	}

	var maxSort sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT MAX(sort_order) FROM diary_blocks WHERE diary_day_id = ?", dayID,
	).Scan(&maxSort); err != nil {
		return false, fmt.Errorf("finding max sort order: %w", err)
	}

	nextSort := int64(-1)
	if maxSort.Valid {
		nextSort = maxSort.Int64
	}

	for i, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		if b.Content == nil || *b.Content == "" {
			continue
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO diary_blocks (diary_day_id, type, content, sort_order)
			 VALUES (?, 'text', ?, ?)`,
			dayID, b.Content, nextSort+1+int64(i),
		); err != nil {
			return false, fmt.Errorf("inserting text block %d: %w", i, err)
		}
	}

	var remaining int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM diary_blocks WHERE diary_day_id = ?", dayID,
	).Scan(&remaining); err != nil {
		return false, fmt.Errorf("counting remaining blocks: %w", err)
	}

	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM diary_days WHERE id = ?", dayID); err != nil {
			return false, fmt.Errorf("removing empty diary day: %w", err)
		}

		removed = true
	} else {
		if _, err := tx.ExecContext(ctx,
			"UPDATE diary_days SET updated_at = CURRENT_TIMESTAMP WHERE id = ?",
			dayID); err != nil {
			return false, fmt.Errorf("touching diary day: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing diary day autosave: %w", err)
	}

	return removed, nil
}

// DeleteDiaryDay removes a snapshot day and returns the file paths its media
// blocks referenced so the caller can clean up now-unreferenced uploads.
func (s *Store) DeleteDiaryDay(
	ctx context.Context, id int64,
) ([]string, error) {
	paths, err := s.collectFilePaths(ctx,
		`SELECT DISTINCT file_path FROM diary_blocks
		 WHERE diary_day_id = ? AND file_path IS NOT NULL AND file_path != ''`,
		id)
	if err != nil {
		return nil, err
	}

	if _, err := s.write.ExecContext(ctx,
		"DELETE FROM diary_days WHERE id = ?", id); err != nil {
		return nil, fmt.Errorf("deleting diary day %d: %w", id, err)
	}

	return paths, nil
}

// DeleteDiaryBlock removes one snapshot block; a day left empty is deleted
// with it.
func (s *Store) DeleteDiaryBlock(ctx context.Context, id int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning diary block delete: %w", err)
	}

	defer tx.Rollback()

	var dayID int64
	if err := tx.QueryRowContext(ctx,
		"SELECT diary_day_id FROM diary_blocks WHERE id = ?", id,
	).Scan(&dayID); err != nil {
		return fmt.Errorf("finding diary block %d: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM diary_blocks WHERE id = ?", id); err != nil {
		return fmt.Errorf("deleting diary block %d: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM diary_days WHERE id = ?
		 AND NOT EXISTS (SELECT 1 FROM diary_blocks WHERE diary_day_id = ?)`,
		dayID, dayID); err != nil {
		return fmt.Errorf("removing empty diary day: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing diary block delete: %w", err)
	}

	return nil
}

// DeleteDiarySection detaches a member's section, returning the file paths
// its blocks referenced for upload cleanup.
func (s *Store) DeleteDiarySection(
	ctx context.Context, id int64,
) ([]string, error) {
	paths, err := s.collectFilePaths(ctx,
		`SELECT DISTINCT b.file_path
		 FROM diary_blocks b
		 JOIN diary_days d ON d.id = b.diary_day_id
		 WHERE d.section_id = ? AND b.file_path IS NOT NULL AND b.file_path != ''`,
		id)
	if err != nil {
		return nil, err
	}

	if _, err := s.write.ExecContext(ctx,
		"DELETE FROM diary_sections WHERE id = ?", id); err != nil {
		return nil, fmt.Errorf("deleting diary section %d: %w", id, err)
	}

	return paths, nil
}

// scanDiarySection reads one diary_sections row.
func (s *Store) scanDiarySection(
	row interface{ Scan(dest ...any) error },
) (*DiarySection, error) {
	var d DiarySection

	err := row.Scan(&d.ID, &d.IssueID, &d.UserID, &d.CopiedThrough,
		&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("scanning diary section: %w", err)
	}

	return &d, nil
}

// collectFilePaths runs a single-column query and gathers non-empty paths.
func (s *Store) collectFilePaths(
	ctx context.Context, query string, args ...any,
) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("collecting file paths: %w", err)
	}
	defer rows.Close()

	paths := make([]string, 0, 8)

	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scanning file path: %w", err)
		}

		if p.Valid && p.String != "" {
			paths = append(paths, p.String)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating file paths: %w", err)
	}

	return paths, nil
}

// copyDiaryDays snapshots journal days in [fromDay, throughDay] (inclusive
// bounds, lexicographic) into a section, skipping days the section already
// has. Returns how many days were copied.
func copyDiaryDays(
	ctx context.Context, tx *sql.Tx,
	sectionID, userID int64, fromDay, throughDay string,
) (int, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO diary_days (section_id, day)
		 SELECT ?, r.day FROM rambles r
		 WHERE r.user_id = ? AND r.day >= ? AND r.day <= ?
		   AND EXISTS (SELECT 1 FROM ramble_blocks rb WHERE rb.ramble_id = r.id)
		   AND NOT EXISTS (SELECT 1 FROM diary_days dd
		                   WHERE dd.section_id = ? AND dd.day = r.day)`,
		sectionID, userID, fromDay, throughDay, sectionID,
	)
	if err != nil {
		return 0, fmt.Errorf("copying diary days: %w", err)
	}

	copied, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting copied diary days: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO diary_blocks
		     (diary_day_id, type, content, file_path, caption, sort_order)
		 SELECT dd.id, rb.type, rb.content, rb.file_path, rb.caption, rb.sort_order
		 FROM diary_days dd
		 JOIN rambles r ON r.user_id = ? AND r.day = dd.day
		 JOIN ramble_blocks rb ON rb.ramble_id = r.id
		 WHERE dd.section_id = ?
		   AND NOT EXISTS (SELECT 1 FROM diary_blocks db
		                   WHERE db.diary_day_id = dd.id)`,
		userID, sectionID,
	); err != nil {
		return 0, fmt.Errorf("copying diary blocks: %w", err)
	}

	return int(copied), nil
}
