package store

import (
	"context"
	"fmt"
	"time"
)

// Comment represents a comment posted on a response.
type Comment struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	ResponseID int64     `json:"response_id"`
	ParentID   *int64    `json:"parent_id"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// CommentWithUser bundles a comment with the author's public profile fields.
type CommentWithUser struct {
	Comment

	AuthorName      string  `json:"author_name"`
	AuthorAvatarURL *string `json:"author_avatar_url"`
}

// CreateComment inserts a new comment and returns its ID.
func (s *Store) CreateComment(
	ctx context.Context,
	userID, responseID int64, parentID *int64, body string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO comments (user_id, response_id, parent_id, body)
		 VALUES (?, ?, ?, ?)`,
		userID, responseID, parentID, body,
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

// GetCommentByID returns a comment by its database ID.
func (s *Store) GetCommentByID(
	ctx context.Context, id int64,
) (*Comment, error) {
	var c Comment

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, response_id, parent_id, body, created_at
		 FROM comments WHERE id = ?`, id,
	).Scan(&c.ID, &c.UserID, &c.ResponseID, &c.ParentID,
		&c.Body, &c.CreatedAt)
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
	rows, err := s.read.QueryContext(ctx,
		`SELECT c.id, c.user_id, c.response_id, c.parent_id, c.body, c.created_at,
		        u.name, u.avatar_url
		 FROM comments c
		 JOIN users u ON c.user_id = u.id
		 WHERE c.response_id = ?
		 ORDER BY (c.parent_id IS NOT NULL), c.created_at ASC`, responseID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing comments for response %d: %w", responseID, err)
	}

	defer rows.Close()

	comments := make([]CommentWithUser, 0)

	for rows.Next() {
		var c CommentWithUser

		err := rows.Scan(&c.ID, &c.UserID, &c.ResponseID, &c.ParentID,
			&c.Body, &c.CreatedAt, &c.AuthorName, &c.AuthorAvatarURL)
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
