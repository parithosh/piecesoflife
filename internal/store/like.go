package store

import (
	"context"
	"fmt"
	"strings"
)

// LikeSummary aggregates likes on a single answer or comment from the
// perspective of a specific viewer.
type LikeSummary struct {
	Count int      `json:"count"`
	Liked bool     `json:"liked"`
	Names []string `json:"names,omitempty"`
}

// ToggleResponseLike toggles userID's like on responseID and returns the
// updated LikeSummary.
func (s *Store) ToggleResponseLike(
	ctx context.Context, userID, responseID int64,
) (LikeSummary, error) {
	return s.toggleLike(ctx, "response_id", userID, responseID)
}

// ToggleCommentLike toggles userID's like on commentID and returns the
// updated LikeSummary.
func (s *Store) ToggleCommentLike(
	ctx context.Context, userID, commentID int64,
) (LikeSummary, error) {
	return s.toggleLike(ctx, "comment_id", userID, commentID)
}

func (s *Store) toggleLike(
	ctx context.Context, column string, userID, targetID int64,
) (LikeSummary, error) {
	// column is one of two compile-time constants ("response_id", "comment_id").
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return LikeSummary{}, fmt.Errorf("beginning like toggle tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	res, err := tx.ExecContext(ctx,
		`DELETE FROM likes WHERE user_id = ? AND `+column+` = ?`,
		userID, targetID,
	)
	if err != nil {
		return LikeSummary{}, fmt.Errorf("deleting like on %s %d: %w", column, targetID, err)
	}

	removed, err := res.RowsAffected()
	if err != nil {
		return LikeSummary{}, fmt.Errorf("checking like delete rows: %w", err)
	}

	if removed == 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO likes (user_id, `+column+`) VALUES (?, ?)`,
			userID, targetID,
		); err != nil {
			return LikeSummary{}, fmt.Errorf("inserting like on %s %d: %w", column, targetID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return LikeSummary{}, fmt.Errorf("committing like toggle: %w", err)
	}

	return s.getLikeSummary(ctx, column, targetID, userID)
}

func (s *Store) getLikeSummary(
	ctx context.Context, column string, targetID, viewerID int64,
) (LikeSummary, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT l.user_id, u.name
		 FROM likes l
		 JOIN users u ON u.id = l.user_id
		 WHERE l.`+column+` = ?
		 ORDER BY l.created_at ASC, l.id ASC`,
		targetID,
	)
	if err != nil {
		return LikeSummary{}, fmt.Errorf("reading likes for %s %d: %w", column, targetID, err)
	}
	defer rows.Close()

	var sum LikeSummary
	for rows.Next() {
		var (
			uid  int64
			name string
		)
		if err := rows.Scan(&uid, &name); err != nil {
			return LikeSummary{}, fmt.Errorf("scanning like row: %w", err)
		}
		sum.Count++
		sum.Names = append(sum.Names, name)
		if uid == viewerID {
			sum.Liked = true
		}
	}
	if err := rows.Err(); err != nil {
		return LikeSummary{}, fmt.Errorf("iterating like rows: %w", err)
	}

	return sum, nil
}

// ListResponseLikesByIssue returns like summaries keyed by response_id for
// every submitted response in issueID.
func (s *Store) ListResponseLikesByIssue(
	ctx context.Context, issueID, viewerID int64,
) (map[int64]LikeSummary, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT l.response_id, l.user_id, u.name
		 FROM likes l
		 JOIN responses r ON r.id = l.response_id
		 JOIN questions q ON q.id = r.question_id
		 JOIN users u ON u.id = l.user_id
		 WHERE q.issue_id = ? AND r.is_draft = 0
		 ORDER BY l.created_at ASC, l.id ASC`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing response likes for issue %d: %w", issueID, err)
	}
	defer rows.Close()

	out := make(map[int64]LikeSummary)
	for rows.Next() {
		var (
			respID int64
			uid    int64
			name   string
		)
		if err := rows.Scan(&respID, &uid, &name); err != nil {
			return nil, fmt.Errorf("scanning response like: %w", err)
		}
		cur := out[respID]
		cur.Count++
		cur.Names = append(cur.Names, name)
		if uid == viewerID {
			cur.Liked = true
		}
		out[respID] = cur
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating response likes: %w", err)
	}

	return out, nil
}

// ListCommentLikesByIDs returns like summaries keyed by comment_id for the
// given comment IDs.
func (s *Store) ListCommentLikesByIDs(
	ctx context.Context, commentIDs []int64, viewerID int64,
) (map[int64]LikeSummary, error) {
	out := make(map[int64]LikeSummary)
	if len(commentIDs) == 0 {
		return out, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(commentIDs)), ",")
	args := make([]any, 0, len(commentIDs))
	for _, id := range commentIDs {
		args = append(args, id)
	}

	rows, err := s.read.QueryContext(ctx,
		`SELECT l.comment_id, l.user_id, u.name
		 FROM likes l
		 JOIN users u ON u.id = l.user_id
		 WHERE l.comment_id IN (`+placeholders+`)
		 ORDER BY l.created_at ASC, l.id ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing comment likes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			commentID int64
			uid       int64
			name      string
		)
		if err := rows.Scan(&commentID, &uid, &name); err != nil {
			return nil, fmt.Errorf("scanning comment like: %w", err)
		}
		cur := out[commentID]
		cur.Count++
		cur.Names = append(cur.Names, name)
		if uid == viewerID {
			cur.Liked = true
		}
		out[commentID] = cur
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating comment likes: %w", err)
	}

	return out, nil
}
