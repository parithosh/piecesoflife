package store

import (
	"context"
	"fmt"
	"time"
)

// seedDefaultQuestionTexts are the default prompts every new Loop starts
// with. Kept in sync with migration 011's seed for group 1.
var seedDefaultQuestionTexts = []string{
	"What good thing happened this month?",
	"What bad thing happened this month?",
	"Free space for random thoughts",
}

// Group is one Loop hosted on this instance. It is deliberately thin: the
// display name and all behaviour live in the group's settings row
// (settings.group_id), so renaming a Loop is just a settings edit.
type Group struct {
	ID        int64     `json:"id"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// GroupOverview is a Loop with the display fields the instance console
// lists: its settings identity plus a member count.
type GroupOverview struct {
	Group
	LoopName      string  `json:"loop_name"`
	Tagline       *string `json:"tagline"`
	SetupComplete bool    `json:"setup_complete"`
	MemberCount   int     `json:"member_count"`
	AdminCount    int     `json:"admin_count"`
}

// CreateGroup creates a Loop with a fresh settings row, the standard
// default questions, and its own copy of the question bank seed. Returns
// the new group's ID.
func (s *Store) CreateGroup(ctx context.Context, loopName string) (int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning group creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, "INSERT INTO groups (is_active) VALUES (1)")
	if err != nil {
		return 0, fmt.Errorf("creating group: %w", err)
	}

	groupID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new group id: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (group_id, loop_name) VALUES (?, ?)`,
		groupID, loopName,
	); err != nil {
		return 0, fmt.Errorf("creating settings for group %d: %w", groupID, err)
	}

	for i, text := range seedDefaultQuestionTexts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO default_questions (group_id, text, sort_order)
			 VALUES (?, ?, ?)`,
			groupID, text, i,
		); err != nil {
			return 0, fmt.Errorf("seeding default questions for group %d: %w", groupID, err)
		}
	}

	if err := seedQuestionBankTx(ctx, tx, groupID); err != nil {
		return 0, fmt.Errorf("seeding question bank for group %d: %w", groupID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing group creation: %w", err)
	}

	return groupID, nil
}

// GetGroup returns a group by ID.
func (s *Store) GetGroup(ctx context.Context, id int64) (*Group, error) {
	var g Group

	err := s.read.QueryRowContext(ctx,
		`SELECT id, is_active, created_at FROM groups WHERE id = ?`, id,
	).Scan(&g.ID, &g.IsActive, &g.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting group %d: %w", id, err)
	}

	return &g, nil
}

// ListGroupOverviews returns every group (active and archived) with its
// settings identity and member counts, for the instance console.
func (s *Store) ListGroupOverviews(ctx context.Context) ([]GroupOverview, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT g.id, g.is_active, g.created_at,
		        st.loop_name, st.tagline, st.setup_complete,
		        (SELECT COUNT(*) FROM memberships m
		          JOIN users u ON u.id = m.user_id
		          WHERE m.group_id = g.id AND m.is_active = 1 AND u.is_active = 1),
		        (SELECT COUNT(*) FROM memberships m
		          JOIN users u ON u.id = m.user_id
		          WHERE m.group_id = g.id AND m.is_active = 1 AND u.is_active = 1
		            AND m.role = 'admin')
		 FROM groups g
		 JOIN settings st ON st.group_id = g.id
		 ORDER BY g.created_at, g.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing group overviews: %w", err)
	}
	defer rows.Close()

	overviews := make([]GroupOverview, 0, 4)

	for rows.Next() {
		var ov GroupOverview

		if err := rows.Scan(&ov.ID, &ov.IsActive, &ov.CreatedAt,
			&ov.LoopName, &ov.Tagline, &ov.SetupComplete,
			&ov.MemberCount, &ov.AdminCount,
		); err != nil {
			return nil, fmt.Errorf("scanning group overview: %w", err)
		}

		overviews = append(overviews, ov)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating group overviews: %w", err)
	}

	return overviews, nil
}

// GetOldestActiveGroupID returns the instance's first-created active
// group — the "default" Loop for legacy data that predates multi-group.
func (s *Store) GetOldestActiveGroupID(ctx context.Context) (int64, error) {
	var id int64

	err := s.read.QueryRowContext(ctx,
		`SELECT id FROM groups WHERE is_active = 1
		 ORDER BY created_at, id LIMIT 1`,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("getting oldest active group: %w", err)
	}

	return id, nil
}

// CountActiveGroups returns how many non-archived groups exist.
func (s *Store) CountActiveGroups(ctx context.Context) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM groups WHERE is_active = 1",
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting active groups: %w", err)
	}

	return count, nil
}

// SetGroupActive archives (false) or restores (true) a group. Archived
// groups keep all their data but disappear from switchers and stop being a
// valid current Loop.
func (s *Store) SetGroupActive(ctx context.Context, id int64, active bool) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE groups SET is_active = ? WHERE id = ?", active, id,
	)
	if err != nil {
		return fmt.Errorf("setting group %d active=%t: %w", id, active, err)
	}

	return nil
}
