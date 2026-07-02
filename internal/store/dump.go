package store

import (
	"context"
	"fmt"
	"time"
)

// DumpItem is one loose photo or video a member attached to an issue's
// "photo & video dump" — media that belongs to the round rather than to a
// specific question. Rendered as the collage closer of the published issue.
type DumpItem struct {
	ID          int64     `json:"id"`
	IssueID     int64     `json:"issue_id"`
	UserID      int64     `json:"user_id"`
	Kind        string    `json:"kind"` // "photo" or "video"
	ContentType *string   `json:"content_type"`
	FilePath    string    `json:"file_path"`
	Caption     *string   `json:"caption"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
}

// DumpItemWithUser bundles a dump item with its contributor's public
// profile fields for the published collage.
type DumpItemWithUser struct {
	DumpItem

	UserName      string  `json:"user_name"`
	UserAvatarURL *string `json:"user_avatar_url"`
}

// CreateDumpItem inserts a dump item and returns its ID. sort_order is
// assigned as the next slot within (issue, user).
func (s *Store) CreateDumpItem(
	ctx context.Context,
	issueID, userID int64, kind string,
	contentType *string, filePath string, caption *string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO dump_items
		     (issue_id, user_id, kind, content_type, file_path, caption, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?,
		     (SELECT COALESCE(MAX(sort_order) + 1, 0) FROM dump_items
		      WHERE issue_id = ? AND user_id = ?))`,
		issueID, userID, kind, contentType, filePath, caption,
		issueID, userID,
	)
	if err != nil {
		return 0, fmt.Errorf("creating dump item: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new dump item id: %w", err)
	}

	return id, nil
}

// GetDumpItemByID returns a single dump item.
func (s *Store) GetDumpItemByID(
	ctx context.Context, id int64,
) (*DumpItem, error) {
	var d DumpItem

	err := s.read.QueryRowContext(ctx,
		`SELECT id, issue_id, user_id, kind, content_type, file_path,
		        caption, sort_order, created_at
		 FROM dump_items WHERE id = ?`, id,
	).Scan(&d.ID, &d.IssueID, &d.UserID, &d.Kind, &d.ContentType,
		&d.FilePath, &d.Caption, &d.SortOrder, &d.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting dump item %d: %w", id, err)
	}

	return &d, nil
}

// ListDumpItemsByIssue returns every dump item for an issue joined with the
// contributor's name and avatar, ordered by user then upload order — the
// order the collage groups render in.
func (s *Store) ListDumpItemsByIssue(
	ctx context.Context, issueID int64,
) ([]DumpItemWithUser, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT d.id, d.issue_id, d.user_id, d.kind, d.content_type,
		        d.file_path, d.caption, d.sort_order, d.created_at,
		        u.name, u.avatar_url
		 FROM dump_items d
		 JOIN users u ON u.id = d.user_id
		 WHERE d.issue_id = ?
		 ORDER BY u.name COLLATE NOCASE, d.sort_order`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing dump items for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	items := make([]DumpItemWithUser, 0)

	for rows.Next() {
		var d DumpItemWithUser
		if err := rows.Scan(&d.ID, &d.IssueID, &d.UserID, &d.Kind,
			&d.ContentType, &d.FilePath, &d.Caption, &d.SortOrder,
			&d.CreatedAt, &d.UserName, &d.UserAvatarURL); err != nil {
			return nil, fmt.Errorf("scanning dump item: %w", err)
		}

		items = append(items, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dump items: %w", err)
	}

	return items, nil
}

// ListDumpItemsForUser returns one member's dump items for an issue in
// upload order — the respond editor's own-items grid.
func (s *Store) ListDumpItemsForUser(
	ctx context.Context, issueID, userID int64,
) ([]DumpItem, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, issue_id, user_id, kind, content_type, file_path,
		        caption, sort_order, created_at
		 FROM dump_items
		 WHERE issue_id = ? AND user_id = ?
		 ORDER BY sort_order`,
		issueID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing dump items for user %d issue %d: %w",
			userID, issueID, err)
	}
	defer rows.Close()

	items := make([]DumpItem, 0)

	for rows.Next() {
		var d DumpItem
		if err := rows.Scan(&d.ID, &d.IssueID, &d.UserID, &d.Kind,
			&d.ContentType, &d.FilePath, &d.Caption, &d.SortOrder,
			&d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning dump item: %w", err)
		}

		items = append(items, d)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating dump items: %w", err)
	}

	return items, nil
}

// CountDumpItemsForUser returns how many items of a kind a member has in an
// issue's dump — enforces the per-member upload caps.
func (s *Store) CountDumpItemsForUser(
	ctx context.Context, issueID, userID int64, kind string,
) (int, error) {
	var n int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dump_items
		 WHERE issue_id = ? AND user_id = ? AND kind = ?`,
		issueID, userID, kind,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting dump items: %w", err)
	}

	return n, nil
}

// DeleteDumpItem removes a dump item and reports whether a row was deleted.
// Ownership and issue-status checks live in the handler.
func (s *Store) DeleteDumpItem(ctx context.Context, id int64) (bool, error) {
	result, err := s.write.ExecContext(ctx,
		`DELETE FROM dump_items WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("deleting dump item %d: %w", id, err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking dump item delete: %w", err)
	}

	return n > 0, nil
}
