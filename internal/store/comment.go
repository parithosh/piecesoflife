package store

import (
	"context"
	"fmt"
	"time"
)

// Comment represents a comment posted on a response, a published notebook
// (diary) day, a single notebook block, or a dump item — exactly one target
// is set. Day-level notebook comments predate per-block threads and live on
// in issues published before migration 021. EditedAt is nil until the author
// edits the body.
type Comment struct {
	ID           int64      `json:"id"`
	UserID       int64      `json:"user_id"`
	ResponseID   *int64     `json:"response_id"`
	DiaryDayID   *int64     `json:"diary_day_id"`
	DiaryBlockID *int64     `json:"diary_block_id"`
	DumpItemID   *int64     `json:"dump_item_id"`
	ParentID     *int64     `json:"parent_id"`
	Body         string     `json:"body"`
	EditedAt     *time.Time `json:"edited_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// CommentWithUser bundles a comment with the author's public profile fields.
type CommentWithUser struct {
	Comment

	AuthorName      string  `json:"author_name"`
	AuthorAvatarURL *string `json:"author_avatar_url"`
}

// CreateComment inserts a new comment on a response and returns its ID.
func (s *Store) CreateComment(
	ctx context.Context,
	userID, responseID int64, parentID *int64, body string,
) (int64, error) {
	return s.createComment(ctx, userID, &responseID, nil, nil, nil, parentID, body)
}

// CreateDiaryComment inserts a new comment on a notebook day and returns
// its ID. Day-level threads are legacy — new conversations start on blocks
// via CreateDiaryBlockComment; this remains so replies can land in threads
// opened before per-block comments existed.
func (s *Store) CreateDiaryComment(
	ctx context.Context,
	userID, diaryDayID int64, parentID *int64, body string,
) (int64, error) {
	return s.createComment(ctx, userID, nil, &diaryDayID, nil, nil, parentID, body)
}

// CreateDiaryBlockComment inserts a new comment on a single published
// notebook block and returns its ID.
func (s *Store) CreateDiaryBlockComment(
	ctx context.Context,
	userID, diaryBlockID int64, parentID *int64, body string,
) (int64, error) {
	return s.createComment(ctx, userID, nil, nil, &diaryBlockID, nil, parentID, body)
}

// CreateDumpComment inserts a new comment on a dump item and returns its ID.
func (s *Store) CreateDumpComment(
	ctx context.Context,
	userID, dumpItemID int64, parentID *int64, body string,
) (int64, error) {
	return s.createComment(ctx, userID, nil, nil, nil, &dumpItemID, parentID, body)
}

// UpdateCommentBody replaces a comment's body and stamps it edited.
// Ownership checks live in the handler.
func (s *Store) UpdateCommentBody(
	ctx context.Context, id int64, body string,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE comments SET body = ?, edited_at = CURRENT_TIMESTAMP
		 WHERE id = ?`, body, id,
	)
	if err != nil {
		return fmt.Errorf("updating comment %d: %w", id, err)
	}

	return nil
}

// GetCommentByID returns a comment by its database ID.
func (s *Store) GetCommentByID(
	ctx context.Context, id int64,
) (*Comment, error) {
	var c Comment

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, response_id, diary_day_id, diary_block_id,
		        dump_item_id, parent_id, body, edited_at, created_at
		 FROM comments WHERE id = ?`, id,
	).Scan(&c.ID, &c.UserID, &c.ResponseID, &c.DiaryDayID, &c.DiaryBlockID,
		&c.DumpItemID, &c.ParentID, &c.Body, &c.EditedAt, &c.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting comment %d: %w", id, err)
	}

	return &c, nil
}

// ListCommentsByResponse returns comments on a response with author details.
// Ordering: top-level comments first (parent_id IS NULL), then replies by
// created_at ascending. Callers build a tree view from this flat list.
func (s *Store) ListCommentsByResponse(
	ctx context.Context, responseID int64,
) ([]CommentWithUser, error) {
	return s.listCommentsByTarget(ctx, "response_id", responseID)
}

// ListCommentsByDiaryDay returns comments on a published notebook day with
// author details, in the same ordering as response comments. Day-level
// threads are legacy (pre-021) — new threads live on blocks.
func (s *Store) ListCommentsByDiaryDay(
	ctx context.Context, diaryDayID int64,
) ([]CommentWithUser, error) {
	return s.listCommentsByTarget(ctx, "diary_day_id", diaryDayID)
}

// ListCommentsByDiaryBlock returns comments on a single published notebook
// block with author details, in the same ordering as response comments.
func (s *Store) ListCommentsByDiaryBlock(
	ctx context.Context, diaryBlockID int64,
) ([]CommentWithUser, error) {
	return s.listCommentsByTarget(ctx, "diary_block_id", diaryBlockID)
}

// CountCommentsByDiaryBlocks returns comment counts for every notebook
// block in an issue's spread, keyed by block ID — one query for the whole
// page instead of one per block. Blocks without comments are absent from
// the map.
func (s *Store) CountCommentsByDiaryBlocks(
	ctx context.Context, issueID int64,
) (map[int64]int, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT c.diary_block_id, COUNT(*)
		 FROM comments c
		 JOIN diary_blocks db ON db.id = c.diary_block_id
		 JOIN diary_days dd ON dd.id = db.diary_day_id
		 JOIN diary_sections ds ON ds.id = dd.section_id
		 WHERE ds.issue_id = ?
		 GROUP BY c.diary_block_id`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("counting diary block comments: %w", err)
	}

	defer rows.Close()

	counts := make(map[int64]int, 32)

	for rows.Next() {
		var (
			blockID int64
			n       int
		)

		if err := rows.Scan(&blockID, &n); err != nil {
			return nil, fmt.Errorf("scanning diary block comment count: %w", err)
		}

		counts[blockID] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating diary block comment counts: %w", err)
	}

	return counts, nil
}

// ListCommentsByDumpItem returns comments on a dump photo/video with author
// details, in the same ordering as response comments.
func (s *Store) ListCommentsByDumpItem(
	ctx context.Context, dumpItemID int64,
) ([]CommentWithUser, error) {
	return s.listCommentsByTarget(ctx, "dump_item_id", dumpItemID)
}

// DeleteComment removes a comment by ID.
func (s *Store) DeleteComment(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM comments WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting comment %d: %w", id, err)
	}

	return nil
}

// createComment inserts a comment on exactly one target.
func (s *Store) createComment(
	ctx context.Context,
	userID int64,
	responseID, diaryDayID, diaryBlockID, dumpItemID, parentID *int64,
	body string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO comments
		     (user_id, response_id, diary_day_id, diary_block_id,
		      dump_item_id, parent_id, body)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, responseID, diaryDayID, diaryBlockID, dumpItemID, parentID, body,
	)
	if err != nil {
		return 0, fmt.Errorf("creating comment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new comment id: %w", err)
	}

	return id, nil
}

// listCommentsByTarget returns comments on one target column with author
// details. Ordering: top-level comments first, then replies by created_at
// ascending; callers build the tree from this flat list.
func (s *Store) listCommentsByTarget(
	ctx context.Context, column string, targetID int64,
) ([]CommentWithUser, error) {
	// column is one of four compile-time constants, never user input.
	rows, err := s.read.QueryContext(ctx,
		`SELECT c.id, c.user_id, c.response_id, c.diary_day_id,
		        c.diary_block_id, c.dump_item_id,
		        c.parent_id, c.body, c.edited_at, c.created_at,
		        u.name, u.avatar_url
		 FROM comments c
		 JOIN users u ON c.user_id = u.id
		 WHERE c.`+column+` = ?
		 ORDER BY (c.parent_id IS NOT NULL), c.created_at ASC`, targetID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing comments for %s %d: %w", column, targetID, err)
	}

	defer rows.Close()

	comments := make([]CommentWithUser, 0)

	for rows.Next() {
		var c CommentWithUser

		err := rows.Scan(&c.ID, &c.UserID, &c.ResponseID, &c.DiaryDayID,
			&c.DiaryBlockID, &c.DumpItemID, &c.ParentID, &c.Body,
			&c.EditedAt, &c.CreatedAt,
			&c.AuthorName, &c.AuthorAvatarURL)
		if err != nil {
			return nil, fmt.Errorf("scanning comment: %w", err)
		}

		comments = append(comments, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating comments: %w", err)
	}

	return comments, nil
}
