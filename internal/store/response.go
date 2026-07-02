package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrVersionConflict indicates an optimistic concurrency conflict during autosave.
var ErrVersionConflict = errors.New("version conflict")

// Response represents a user's answer to a question.
type Response struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	QuestionID int64     `json:"question_id"`
	IsDraft    bool      `json:"is_draft"`
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ResponseBlock represents an ordered content block within a response.
type ResponseBlock struct {
	ID         int64     `json:"id"`
	ResponseID int64     `json:"response_id"`
	Type       string    `json:"type"`
	Content    *string   `json:"content"`
	FilePath   *string   `json:"file_path"`
	Caption    *string   `json:"caption"`
	LinkURL    *string   `json:"link_url"`
	SortOrder  int       `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ResponseWithBlocks combines a response with its content blocks and author.
type ResponseWithBlocks struct {
	Response Response        `json:"response"`
	Blocks   []ResponseBlock `json:"blocks"`
	User     User            `json:"user"`
}

// SubmissionProgress tracks which members have responded to an issue.
//
// Admins are excluded from TotalMembers/Responded by default so the ratio
// reflects the friends/family the round is actually waiting on. When the
// issue's CountAdminIn flag is set, admins are folded back into the counts.
// Members always lists everyone (including admins) for the roster display.
type SubmissionProgress struct {
	TotalMembers int              `json:"total_members"`
	Responded    int              `json:"responded"`
	CountAdminIn bool             `json:"count_admin_in"`
	Members      []MemberProgress `json:"members"`
}

// MemberProgress tracks whether an individual member has responded.
type MemberProgress struct {
	User      User `json:"user"`
	Responded bool `json:"responded"`
}

// CreateResponse inserts a new draft response and returns its ID.
func (s *Store) CreateResponse(
	ctx context.Context, userID, questionID int64,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO responses (user_id, question_id)
		 VALUES (?, ?)`,
		userID, questionID,
	)
	if err != nil {
		return 0, fmt.Errorf("creating response: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new response id: %w", err)
	}

	return id, nil
}

// GetResponseByID returns a response by its database ID.
func (s *Store) GetResponseByID(
	ctx context.Context, id int64,
) (*Response, error) {
	var r Response

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, question_id, is_draft, version,
		        created_at, updated_at
		 FROM responses WHERE id = ?`, id,
	).Scan(&r.ID, &r.UserID, &r.QuestionID, &r.IsDraft,
		&r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting response %d: %w", id, err)
	}

	return &r, nil
}

// GetUserResponse returns a user's response to a specific question.
func (s *Store) GetUserResponse(
	ctx context.Context, userID, questionID int64,
) (*Response, error) {
	var r Response

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, question_id, is_draft, version,
		        created_at, updated_at
		 FROM responses WHERE user_id = ? AND question_id = ?`,
		userID, questionID,
	).Scan(&r.ID, &r.UserID, &r.QuestionID, &r.IsDraft,
		&r.Version, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting user response: %w", err)
	}

	return &r, nil
}

// DeleteResponse removes a response by ID.
func (s *Store) DeleteResponse(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM responses WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting response %d: %w", id, err)
	}

	return nil
}

// SubmitResponse marks a response as submitted (not draft).
func (s *Store) SubmitResponse(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE responses SET is_draft = 0,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("submitting response %d: %w", id, err)
	}

	return nil
}

// AutosaveResponse replaces the response's text blocks under optimistic
// concurrency control. Photo and link blocks are NOT touched — those are
// managed exclusively through the /blocks endpoints, and an autosave round
// trip would otherwise wipe them. New text blocks are appended after any
// existing non-text blocks; user-driven reordering happens through the
// reorder endpoint.
//
// Returns the new version number, or (currentVersion, ErrVersionConflict)
// when the optimistic check fails.
func (s *Store) AutosaveResponse(
	ctx context.Context,
	id int64, expectedVersion int, blocks []ResponseBlock,
) (int, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning autosave transaction: %w", err)
	}

	defer tx.Rollback()

	var currentVersion int

	err = tx.QueryRowContext(ctx,
		"SELECT version FROM responses WHERE id = ?", id,
	).Scan(&currentVersion)
	if err != nil {
		return 0, fmt.Errorf("checking response version: %w", err)
	}

	if currentVersion != expectedVersion {
		return currentVersion, ErrVersionConflict
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM response_blocks WHERE response_id = ? AND type = 'text'", id,
	); err != nil {
		return 0, fmt.Errorf("deleting existing text blocks: %w", err)
	}

	// Insert new text blocks after any surviving non-text blocks so
	// sort_orders never collide. -1 ensures the first inserted block lands
	// at 0 when no non-text blocks remain.
	var maxSort sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT MAX(sort_order) FROM response_blocks WHERE response_id = ?", id,
	).Scan(&maxSort); err != nil {
		return 0, fmt.Errorf("finding max sort order: %w", err)
	}

	nextSort := int64(-1)
	if maxSort.Valid {
		nextSort = maxSort.Int64
	}

	for i, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			// Defence in depth — the handler already rejects these.
			continue
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO response_blocks
			 (response_id, type, content, file_path, caption, link_url, sort_order)
			 VALUES (?, 'text', ?, NULL, NULL, NULL, ?)`,
			id, b.Content, nextSort+1+int64(i),
		); err != nil {
			return 0, fmt.Errorf("inserting text block %d: %w", i, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE responses SET version = version + 1,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id,
	); err != nil {
		return 0, fmt.Errorf("incrementing version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing autosave: %w", err)
	}

	return currentVersion + 1, nil
}

// ListPhotosForIssue returns up to `limit` photo file paths from submitted
// responses for an issue, newest first. Used by the archive collage thumbnail.
func (s *Store) ListPhotosForIssue(
	ctx context.Context, issueID int64, limit int,
) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT rb.file_path
		 FROM response_blocks rb
		 JOIN responses r ON rb.response_id = r.id
		 JOIN questions q ON r.question_id = q.id
		 WHERE q.issue_id = ? AND r.is_draft = 0
		       AND rb.type = 'photo' AND rb.file_path IS NOT NULL
		 ORDER BY rb.created_at DESC
		 LIMIT ?`, issueID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("listing photos for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	paths := make([]string, 0, limit)

	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scanning photo path: %w", err)
		}
		paths = append(paths, p)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating photos: %w", err)
	}

	return paths, nil
}

// ListResponsesByIssue returns submitted responses for an issue.
func (s *Store) ListResponsesByIssue(
	ctx context.Context, issueID int64, onlySubmitted bool,
) ([]Response, error) {
	query := `SELECT r.id, r.user_id, r.question_id, r.is_draft,
	                  r.version, r.created_at, r.updated_at
	           FROM responses r
	           JOIN questions q ON r.question_id = q.id
	           WHERE q.issue_id = ?`

	if onlySubmitted {
		query += " AND r.is_draft = 0"
	}

	query += " ORDER BY q.sort_order, r.user_id"

	rows, err := s.read.QueryContext(ctx, query, issueID)
	if err != nil {
		return nil, fmt.Errorf("listing responses for issue %d: %w", issueID, err)
	}

	defer rows.Close()

	var responses []Response

	for rows.Next() {
		var r Response

		err := rows.Scan(&r.ID, &r.UserID, &r.QuestionID, &r.IsDraft,
			&r.Version, &r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning response: %w", err)
		}

		responses = append(responses, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating responses: %w", err)
	}

	if responses == nil {
		responses = make([]Response, 0)
	}

	return responses, nil
}

// ListUserResponsesForIssue returns all responses by a user for an issue.
func (s *Store) ListUserResponsesForIssue(
	ctx context.Context, userID, issueID int64,
) ([]Response, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT r.id, r.user_id, r.question_id, r.is_draft,
		        r.version, r.created_at, r.updated_at
		 FROM responses r
		 JOIN questions q ON r.question_id = q.id
		 WHERE q.issue_id = ? AND r.user_id = ?
		 ORDER BY q.sort_order`,
		issueID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing user responses: %w", err)
	}

	defer rows.Close()

	var responses []Response

	for rows.Next() {
		var r Response

		err := rows.Scan(&r.ID, &r.UserID, &r.QuestionID, &r.IsDraft,
			&r.Version, &r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning user response: %w", err)
		}

		responses = append(responses, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user responses: %w", err)
	}

	if responses == nil {
		responses = make([]Response, 0)
	}

	return responses, nil
}

// GetSubmissionProgress returns who has responded to an issue.
//
// The returned Members roster includes everyone (admins too), but admins are
// only counted toward TotalMembers/Responded when the issue's count_admin_in
// flag is set. Admin rows sort last so the roster leads with the members the
// round is waiting on.
func (s *Store) GetSubmissionProgress(
	ctx context.Context, issueID int64,
) (*SubmissionProgress, error) {
	var countAdminIn bool
	if err := s.read.QueryRowContext(ctx,
		"SELECT count_admin_in FROM issues WHERE id = ?", issueID,
	).Scan(&countAdminIn); err != nil {
		return nil, fmt.Errorf("getting count_admin_in for issue %d: %w", issueID, err)
	}

	rows, err := s.read.QueryContext(ctx,
		`SELECT u.id, u.name, u.email, u.avatar_url, u.bio,
		        u.role, u.is_active, u.created_at,
		        EXISTS(
		            SELECT 1 FROM responses r
		            JOIN questions q ON r.question_id = q.id
		            WHERE q.issue_id = ? AND r.user_id = u.id
		            AND r.is_draft = 0
		        ) AS responded
		 FROM users u
		 WHERE u.is_active = 1
		 ORDER BY (u.role = 'admin'), u.name`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("getting submission progress: %w", err)
	}

	defer rows.Close()

	progress := &SubmissionProgress{
		CountAdminIn: countAdminIn,
		Members:      make([]MemberProgress, 0),
	}

	for rows.Next() {
		var mp MemberProgress

		err := rows.Scan(&mp.User.ID, &mp.User.Name, &mp.User.Email,
			&mp.User.AvatarURL, &mp.User.Bio, &mp.User.Role,
			&mp.User.IsActive, &mp.User.CreatedAt, &mp.Responded)
		if err != nil {
			return nil, fmt.Errorf("scanning member progress: %w", err)
		}

		progress.Members = append(progress.Members, mp)

		// Admins only count toward the ratio when the round opts them in.
		if mp.User.Role != "admin" || countAdminIn {
			progress.TotalMembers++

			if mp.Responded {
				progress.Responded++
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating member progress: %w", err)
	}

	return progress, nil
}

// SetIssueCountAdminIn toggles whether admins are folded into an issue's
// progress denominator.
func (s *Store) SetIssueCountAdminIn(
	ctx context.Context, issueID int64, on bool,
) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE issues SET count_admin_in = ? WHERE id = ?", on, issueID,
	)
	if err != nil {
		return fmt.Errorf("setting count_admin_in for issue %d: %w", issueID, err)
	}

	return nil
}

// CountBlocksForResponseType counts blocks of a given type in a response.
func (s *Store) CountBlocksForResponseType(
	ctx context.Context, responseID int64, blockType string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM response_blocks
		 WHERE response_id = ? AND type = ?`, responseID, blockType,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting %s blocks: %w", blockType, err)
	}

	return count, nil
}

// CreateBlock inserts a new content block for a response.
func (s *Store) CreateBlock(
	ctx context.Context,
	responseID int64, blockType string,
	content, filePath, caption, linkURL *string,
	sortOrder int,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO response_blocks
		 (response_id, type, content, file_path, caption, link_url, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		responseID, blockType, content, filePath, caption, linkURL, sortOrder,
	)
	if err != nil {
		return 0, fmt.Errorf("creating block: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new block id: %w", err)
	}

	return id, nil
}

// GetBlockByID returns a response block by its database ID.
func (s *Store) GetBlockByID(
	ctx context.Context, id int64,
) (*ResponseBlock, error) {
	var b ResponseBlock

	err := s.read.QueryRowContext(ctx,
		`SELECT id, response_id, type, content, file_path, caption,
		        link_url, sort_order, created_at, updated_at
		 FROM response_blocks WHERE id = ?`, id,
	).Scan(&b.ID, &b.ResponseID, &b.Type, &b.Content,
		&b.FilePath, &b.Caption, &b.LinkURL, &b.SortOrder,
		&b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting block %d: %w", id, err)
	}

	return &b, nil
}

// ListBlocksByResponse returns all blocks for a response in order.
func (s *Store) ListBlocksByResponse(
	ctx context.Context, responseID int64,
) ([]ResponseBlock, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, response_id, type, content, file_path, caption,
		        link_url, sort_order, created_at, updated_at
		 FROM response_blocks WHERE response_id = ?
		 ORDER BY sort_order`, responseID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing blocks for response %d: %w", responseID, err)
	}

	defer rows.Close()

	var blocks []ResponseBlock

	for rows.Next() {
		var b ResponseBlock

		err := rows.Scan(&b.ID, &b.ResponseID, &b.Type, &b.Content,
			&b.FilePath, &b.Caption, &b.LinkURL, &b.SortOrder,
			&b.CreatedAt, &b.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning block: %w", err)
		}

		blocks = append(blocks, b)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating blocks: %w", err)
	}

	if blocks == nil {
		blocks = make([]ResponseBlock, 0)
	}

	return blocks, nil
}

// UpdateBlock modifies a block's content and caption.
func (s *Store) UpdateBlock(
	ctx context.Context, id int64, content, caption *string,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE response_blocks SET content = ?, caption = ?,
		 updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		content, caption, id,
	)
	if err != nil {
		return fmt.Errorf("updating block %d: %w", id, err)
	}

	return nil
}

// DeleteBlock removes a response block by ID.
func (s *Store) DeleteBlock(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM response_blocks WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting block %d: %w", id, err)
	}

	return nil
}

// ReorderBlocks updates sort_order for blocks in a response.
func (s *Store) ReorderBlocks(
	ctx context.Context, responseID int64, orderedIDs []int64,
) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning block reorder transaction: %w", err)
	}

	defer tx.Rollback()

	for i, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE response_blocks SET sort_order = ?,
			 updated_at = CURRENT_TIMESTAMP
			 WHERE id = ? AND response_id = ?`,
			i, id, responseID,
		); err != nil {
			return fmt.Errorf("updating block sort order %d: %w", id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing block reorder: %w", err)
	}

	return nil
}
