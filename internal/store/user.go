package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// User represents a member of the newsletter group.
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	AvatarURL *string   `json:"avatar_url"`
	Bio       *string   `json:"bio"`
	Role      string    `json:"role"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

// GetUserByID returns a user by their database ID.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	var u User

	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, email, avatar_url, bio, role, is_active, created_at
		 FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.Bio,
		&u.Role, &u.IsActive, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting user by id %d: %w", id, err)
	}

	return &u, nil
}

// GetUserByEmail returns a user by their email address.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User

	err := s.read.QueryRowContext(ctx,
		`SELECT id, name, email, avatar_url, bio, role, is_active, created_at
		 FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.Bio,
		&u.Role, &u.IsActive, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting user by email: %w", err)
	}

	return &u, nil
}

// ListActiveUsers returns all active users.
func (s *Store) ListActiveUsers(ctx context.Context) ([]User, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, email, avatar_url, bio, role, is_active, created_at
		 FROM users WHERE is_active = 1 ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing active users: %w", err)
	}
	defer rows.Close()

	return scanUsers(rows)
}

// ListAllUsers returns all users including deactivated ones (for admin).
func (s *Store) ListAllUsers(ctx context.Context) ([]User, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT id, name, email, avatar_url, bio, role, is_active, created_at
		 FROM users ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing all users: %w", err)
	}
	defer rows.Close()

	return scanUsers(rows)
}

// CreateUser inserts a new user and returns their ID.
func (s *Store) CreateUser(
	ctx context.Context, name, email, role string,
) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		`INSERT INTO users (name, email, role, is_active)
		 VALUES (?, ?, ?, 1)`,
		name, email, role,
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

// DeactivateUser sets a user as inactive and deletes all their sessions.
func (s *Store) DeactivateUser(ctx context.Context, id int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning deactivate transaction: %w", err)
	}

	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"UPDATE users SET is_active = 0 WHERE id = ?", id,
	); err != nil {
		return fmt.Errorf("deactivating user %d: %w", id, err)
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM sessions WHERE user_id = ?", id,
	); err != nil {
		return fmt.Errorf("deleting sessions for user %d: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing deactivation: %w", err)
	}

	return nil
}

// ReactivateUser sets a user as active again.
func (s *Store) ReactivateUser(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE users SET is_active = 1 WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("reactivating user %d: %w", id, err)
	}

	return nil
}

// PromoteToAdmin sets a user's role to admin.
func (s *Store) PromoteToAdmin(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"UPDATE users SET role = 'admin' WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("promoting user %d to admin: %w", id, err)
	}

	return nil
}

func scanUsers(rows *sql.Rows) ([]User, error) {
	var users []User

	for rows.Next() {
		var u User

		err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.Bio,
			&u.Role, &u.IsActive, &u.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}

		users = append(users, u)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating users: %w", err)
	}

	if users == nil {
		users = make([]User, 0)
	}

	return users, nil
}
