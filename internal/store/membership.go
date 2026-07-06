package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// groupMemberSelect is the shared SELECT for GroupMember scans; callers
// append extra conditions and ORDER BY. Effective IsActive is the AND of
// the membership and global user flags.
const groupMemberSelect = `
	SELECT u.id, u.name, u.email, u.avatar_url, u.bio,
	       (m.is_active AND u.is_active), u.is_instance_admin,
	       u.last_group_id, u.created_at,
	       m.role, m.is_active, m.created_at
	FROM memberships m
	JOIN users u ON u.id = m.user_id
	WHERE m.group_id = ?`

// Membership ties a user to one group with a per-group role and active
// flag. A user has at most one membership per group.
type Membership struct {
	ID        int64     `json:"id"`
	GroupID   int64     `json:"group_id"`
	UserID    int64     `json:"user_id"`
	Role      string    `json:"role"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// GroupMember is a user seen through one group's lens: identity fields plus
// their role and active state in that group. IsActive is the effective flag
// (membership AND global user).
type GroupMember struct {
	User
	Role         string    `json:"role"`
	MemberActive bool      `json:"member_active"`
	JoinedAt     time.Time `json:"joined_at"`
}

// UserGroup is one entry of a user's Loop list: the group plus its display
// identity and the user's role in it. Powers the nav switcher and /loops.
type UserGroup struct {
	GroupID       int64   `json:"group_id"`
	LoopName      string  `json:"loop_name"`
	Tagline       *string `json:"tagline"`
	AccentColor   string  `json:"accent_color"`
	Role          string  `json:"role"`
	SetupComplete bool    `json:"setup_complete"`
}

// GetMembership returns a user's membership in a group.
func (s *Store) GetMembership(
	ctx context.Context, groupID, userID int64,
) (*Membership, error) {
	var m Membership

	err := s.read.QueryRowContext(ctx,
		`SELECT id, group_id, user_id, role, is_active, created_at
		 FROM memberships WHERE group_id = ? AND user_id = ?`,
		groupID, userID,
	).Scan(&m.ID, &m.GroupID, &m.UserID, &m.Role, &m.IsActive, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting membership (group %d, user %d): %w",
			groupID, userID, err)
	}

	return &m, nil
}

// CreateMembership adds a user to a group. If a membership already exists
// (active or deactivated) it is reactivated, and the role only ever moves
// up: an existing admin is never demoted by a stray re-invite — demotion
// must go through SetMembershipRole deliberately.
func (s *Store) CreateMembership(
	ctx context.Context, groupID, userID int64, role string,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO memberships (group_id, user_id, role, is_active)
		 VALUES (?, ?, ?, 1)
		 ON CONFLICT(group_id, user_id)
		 DO UPDATE SET
		   role = CASE
		     WHEN memberships.role = 'admin' OR excluded.role = 'admin'
		     THEN 'admin' ELSE excluded.role
		   END,
		   is_active = 1`,
		groupID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("creating membership (group %d, user %d): %w",
			groupID, userID, err)
	}

	return nil
}

// SetMembershipRole changes a user's role within one group.
func (s *Store) SetMembershipRole(
	ctx context.Context, groupID, userID int64, role string,
) error {
	res, err := s.write.ExecContext(ctx,
		"UPDATE memberships SET role = ? WHERE group_id = ? AND user_id = ?",
		role, groupID, userID,
	)
	if err != nil {
		return fmt.Errorf("setting role for user %d in group %d: %w",
			userID, groupID, err)
	}

	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("membership (group %d, user %d): %w",
			groupID, userID, sql.ErrNoRows)
	}

	return nil
}

// SetMembershipActive deactivates or reactivates a user within one group
// only; their account and other Loops are untouched.
func (s *Store) SetMembershipActive(
	ctx context.Context, groupID, userID int64, active bool,
) error {
	res, err := s.write.ExecContext(ctx,
		"UPDATE memberships SET is_active = ? WHERE group_id = ? AND user_id = ?",
		active, groupID, userID,
	)
	if err != nil {
		return fmt.Errorf("setting active=%t for user %d in group %d: %w",
			active, userID, groupID, err)
	}

	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("membership (group %d, user %d): %w",
			groupID, userID, sql.ErrNoRows)
	}

	return nil
}

// ListGroupMembers returns everyone who has ever been added to a group,
// including per-group deactivated members (the admin members page shows
// and reactivates them).
func (s *Store) ListGroupMembers(
	ctx context.Context, groupID int64,
) ([]GroupMember, error) {
	rows, err := s.read.QueryContext(ctx,
		groupMemberSelect+` ORDER BY u.name`, groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing members of group %d: %w", groupID, err)
	}
	defer rows.Close()

	return scanGroupMembers(rows)
}

// ListActiveGroupMembers returns the group's active members — the audience
// for reminders, published issues, and progress rosters.
func (s *Store) ListActiveGroupMembers(
	ctx context.Context, groupID int64,
) ([]GroupMember, error) {
	rows, err := s.read.QueryContext(ctx,
		groupMemberSelect+` AND m.is_active = 1 AND u.is_active = 1
		 ORDER BY u.name`, groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing active members of group %d: %w", groupID, err)
	}
	defer rows.Close()

	return scanGroupMembers(rows)
}

// ListUserGroups returns the active Loops a user belongs to, oldest first —
// the switcher and /loops page source.
func (s *Store) ListUserGroups(
	ctx context.Context, userID int64,
) ([]UserGroup, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT g.id, st.loop_name, st.tagline, st.accent_color,
		        m.role, st.setup_complete
		 FROM memberships m
		 JOIN groups g ON g.id = m.group_id
		 JOIN settings st ON st.group_id = g.id
		 WHERE m.user_id = ? AND m.is_active = 1 AND g.is_active = 1
		 ORDER BY g.created_at, g.id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing groups for user %d: %w", userID, err)
	}
	defer rows.Close()

	groups := make([]UserGroup, 0, 4)

	for rows.Next() {
		var ug UserGroup

		if err := rows.Scan(&ug.GroupID, &ug.LoopName, &ug.Tagline,
			&ug.AccentColor, &ug.Role, &ug.SetupComplete,
		); err != nil {
			return nil, fmt.Errorf("scanning user group: %w", err)
		}

		groups = append(groups, ug)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating user groups: %w", err)
	}

	return groups, nil
}

func scanGroupMembers(rows *sql.Rows) ([]GroupMember, error) {
	members := make([]GroupMember, 0, 16)

	for rows.Next() {
		var gm GroupMember

		err := rows.Scan(&gm.ID, &gm.Name, &gm.Email, &gm.AvatarURL, &gm.Bio,
			&gm.User.IsActive, &gm.IsInstanceAdmin, &gm.LastGroupID,
			&gm.User.CreatedAt, &gm.Role, &gm.MemberActive, &gm.JoinedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning group member: %w", err)
		}

		members = append(members, gm)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating group members: %w", err)
	}

	return members, nil
}
