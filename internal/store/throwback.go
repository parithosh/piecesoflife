package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
)

// ThrowbackPhoto is one uncovered photo from a past issue, shown in the
// "Photos from one year ago" closing section of a published issue.
type ThrowbackPhoto struct {
	FilePath   string  `json:"file_path"`
	Caption    *string `json:"caption"`
	AuthorName string  `json:"author_name"`
}

// FindThrowbackIssue locates the past published issue to draw throwback photos
// from: first a published issue in the same circle from one year earlier
// (same month, year - 1), and if none exists, the 12th earlier published issue
// in edition order. Returns (nil, nil) when neither exists.
func (s *Store) FindThrowbackIssue(
	ctx context.Context, groupID int64, current Issue,
) (*Issue, error) {
	// 1. Exact one-year-ago match (same month, previous year).
	iss, err := scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ? AND status = 'published' AND id != ?
		   AND year = ? AND month = ?
		 ORDER BY published_at DESC, id DESC
		 LIMIT 1`,
		groupID, current.ID, current.Year-1, current.Month,
	))
	if err == nil {
		return iss, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("finding one-year-ago issue: %w", err)
	}

	// 2. Fallback: 12th earlier published issue in edition order (OFFSET 11).
	iss, err = scanIssue(s.read.QueryRowContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE group_id = ? AND status = 'published'
		   AND (year < ?
		        OR (year = ? AND month < ?)
		        OR (year = ? AND month = ? AND id < ?))
		 ORDER BY year DESC, month DESC, id DESC
		 LIMIT 1 OFFSET 11`,
		groupID,
		current.Year,
		current.Year, current.Month,
		current.Year, current.Month, current.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("finding 12-issues-ago fallback issue: %w", err)
	}

	return iss, nil
}

// ListRandomThrowbackPhotos collects every uncovered photo published in
// issueID across answers, the photo dump, and woven notebooks, then returns
// up to limit randomly chosen photos.
func (s *Store) ListRandomThrowbackPhotos(
	ctx context.Context, issueID int64, limit int,
) ([]ThrowbackPhoto, error) {
	if limit <= 0 {
		return make([]ThrowbackPhoto, 0), nil
	}

	rows, err := s.read.QueryContext(ctx,
		`SELECT file_path, caption, author_name FROM (
			SELECT rb.file_path AS file_path, rb.caption AS caption, u.name AS author_name
			FROM response_blocks rb
			JOIN responses r ON r.id = rb.response_id
			JOIN questions q ON q.id = r.question_id
			JOIN users u ON u.id = r.user_id
			WHERE q.issue_id = ? AND r.is_draft = 0
			  AND rb.type = 'photo' AND rb.file_path IS NOT NULL
			  AND rb.is_covered = FALSE

			UNION ALL

			SELECT di.file_path AS file_path, di.caption AS caption, u.name AS author_name
			FROM dump_items di
			JOIN users u ON u.id = di.user_id
			WHERE di.issue_id = ?
			  AND di.kind = 'photo' AND di.file_path IS NOT NULL
			  AND di.is_covered = FALSE

			UNION ALL

			SELECT db.file_path AS file_path, db.caption AS caption, u.name AS author_name
			FROM diary_blocks db
			JOIN diary_days dd ON dd.id = db.diary_day_id
			JOIN diary_sections ds ON ds.id = dd.section_id
			JOIN users u ON u.id = ds.user_id
			WHERE ds.issue_id = ?
			  AND db.type = 'photo' AND db.file_path IS NOT NULL
			  AND db.is_covered = FALSE
		)`,
		issueID, issueID, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing throwback photos for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	seen := make(map[string]struct{}, 16)
	photos := make([]ThrowbackPhoto, 0, 16)

	for rows.Next() {
		var p ThrowbackPhoto
		if err := rows.Scan(&p.FilePath, &p.Caption, &p.AuthorName); err != nil {
			return nil, fmt.Errorf("scanning throwback photo: %w", err)
		}
		if _, dup := seen[p.FilePath]; dup {
			continue
		}
		seen[p.FilePath] = struct{}{}
		photos = append(photos, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating throwback photos: %w", err)
	}

	if len(photos) > 1 {
		rand.Shuffle(len(photos), func(i, j int) {
			photos[i], photos[j] = photos[j], photos[i]
		})
	}

	if len(photos) > limit {
		photos = photos[:limit]
	}

	return photos, nil
}
