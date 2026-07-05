package store

import (
	"context"
	"fmt"
	"time"
)

// User is an instance-global identity: one row per email address, shared by
// every Loop the person belongs to. Per-Loop role and active state live on
// memberships; IsActive here is the global kill switch.
type User struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Email           string    `json:"email"`
	AvatarURL       *string   `json:"avatar_url"`
	Bio             *string   `json:"bio"`
	IsActive        bool      `json:"is_active"`
	IsInstanceAdmin bool      `json:"is_instance_admin"`
	LastGroupID     *int64    `json:"-"`
	CreatedAt       time.Time `json:"created_at"`
}

const userColumns = `id, name, email, avatar_url, bio, is_active,
	is_instance_admin, last_group_id, created_at`

// GetUserByID returns a user by their database ID.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, id,
	))
	if err != nil {
		return nil, fmt.Errorf("getting user by id %d: %w", id, err)
	}

	return u, nil
}

// GetUserByEmail returns a user by their email address.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u, err := scanUser(s.read.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = ?`, email,
	))
	if err != nil {
		return nil, fmt.Errorf("getting user by email: %w", err)
	}

	return u, nil
}

// CreateUser inserts a new user identity and returns their ID. Group
// access is granted separately via CreateMembership.
func (s *Store) CreateUser(
	ctx context.Context, name, email string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO users (name, email, is_active) VALUES (?, ?, 1)`,
		name, email,
	)
	if err != nil {
		return 0, fmt.Errorf("creating user: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting new user id: %w", err)
	}

	return id, nil
}

// UpdateUser updates a user's name, avatar URL, and bio.
func (s *Store) UpdateUser(
	ctx context.Context, id int64, name string, avatarURL, bio *string,
) error {
	_, err := s.write.ExecContext(ctx,
		`UPDATE users SET name = ?, avatar_url = ?, bio = ?
		 WHERE id = ?`,
		name, avatarURL, bio, id,
	)
	if err != nil {
		return fmt.Errorf("updating user %d: %w", id, err)
	}

	return nil
}

// SetUserName updates only a user's display name.
func (s *Store) SetUserName(ctx context.Context, id int64, name string) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE users SET name = ? WHERE id = ?", name, id,
	)
	if err != nil {
		return fmt.Errorf("setting user name for %d: %w", id, err)
	}

	return nil
}

// ReactivateUser sets a user as globally active again.
func (s *Store) ReactivateUser(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE users SET is_active = 1 WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("reactivating user %d: %w", id, err)
	}

	return nil
}

// SetLastGroup remembers the Loop a user most recently worked in, so their
// next login lands there.
func (s *Store) SetLastGroup(ctx context.Context, userID, groupID int64) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE users SET last_group_id = ? WHERE id = ?", groupID, userID,
	)
	if err != nil {
		return fmt.Errorf("setting last group for user %d: %w", userID, err)
	}

	return nil
}

func scanUser(row interface{ Scan(dest ...any) error }) (*User, error) {
	var u User

	err := row.Scan(&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.Bio,
		&u.IsActive, &u.IsInstanceAdmin, &u.LastGroupID, &u.CreatedAt)
	if err != nil {
		return nil, err
	}

	return &u, nil
}
