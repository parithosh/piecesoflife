package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrTokenAlreadyConsumed is returned when ConsumeAuthToken is called on
// a token that has already been consumed (a losing race).
var ErrTokenAlreadyConsumed = errors.New("auth token already consumed")

// AuthToken represents a magic link or email CTA authentication token.
type AuthToken struct {
	ID         int64      `json:"-"`
	UserID     int64      `json:"-"`
	TokenHash  string     `json:"-"`
	Type       string     `json:"-"`
	ExpiresAt  time.Time  `json:"-"`
	ConsumedAt *time.Time `json:"-"`
	CreatedAt  time.Time  `json:"-"`
}

// Session represents a server-side user session.
type Session struct {
	ID        int64     `json:"-"`
	UserID    int64     `json:"-"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"-"`
	CreatedAt time.Time `json:"-"`
}

// CreateAuthToken stores a new authentication token hash.
func (s *Store) CreateAuthToken(
	ctx context.Context,
	userID int64, tokenHash, tokenType string, expiresAt time.Time,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO auth_tokens (user_id, token_hash, type, expires_at)
		 VALUES (?, ?, ?, ?)`,
		userID, tokenHash, tokenType, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("creating auth token: %w", err)
	}

	return nil
}

// GetAuthTokenByHash retrieves an auth token by its SHA-256 hash.
func (s *Store) GetAuthTokenByHash(
	ctx context.Context, tokenHash string,
) (*AuthToken, error) {
	var t AuthToken

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, token_hash, type, expires_at, consumed_at, created_at
		 FROM auth_tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&t.ID, &t.UserID, &t.TokenHash, &t.Type,
		&t.ExpiresAt, &t.ConsumedAt, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting auth token by hash: %w", err)
	}

	return &t, nil
}

// ConsumeAuthToken atomically marks a token as consumed. Returns
// ErrTokenAlreadyConsumed if the token was already consumed (race loser)
// so callers don't mint a second session off the same magic link.
func (s *Store) ConsumeAuthToken(ctx context.Context, id int64) error {
	result, err := s.write.ExecContext(ctx,
		`UPDATE auth_tokens SET consumed_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND consumed_at IS NULL`, id,
	)
	if err != nil {
		return fmt.Errorf("consuming auth token %d: %w", id, err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking consume result for token %d: %w", id, err)
	}

	if n == 0 {
		return ErrTokenAlreadyConsumed
	}

	return nil
}

// CleanupExpiredTokens deletes consumed or expired auth tokens.
func (s *Store) CleanupExpiredTokens(ctx context.Context) (int64, error) {
	// Login attempts only matter for the one-hour rate window; a day of
	// slack keeps the table tiny without racing the limiter.
	if _, err := s.write.ExecContext(ctx,
		`DELETE FROM login_attempts
		 WHERE created_at < datetime('now', '-1 day')`,
	); err != nil {
		return 0, fmt.Errorf("cleaning up login attempts: %w", err)
	}

	result, err := s.write.ExecContext(ctx,
		`DELETE FROM auth_tokens
		 WHERE consumed_at IS NOT NULL OR expires_at < CURRENT_TIMESTAMP`,
	)
	if err != nil {
		return 0, fmt.Errorf("cleaning up expired tokens: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("getting cleanup count: %w", err)
	}

	return n, nil
}

// CreateSession stores a new session token hash.
func (s *Store) CreateSession(
	ctx context.Context,
	userID int64, tokenHash string, expiresAt time.Time,
) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO sessions (user_id, token_hash, expires_at)
		 VALUES (?, ?, ?)`,
		userID, tokenHash, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	return nil
}

// GetSessionByHash retrieves a session by its token hash.
func (s *Store) GetSessionByHash(
	ctx context.Context, tokenHash string,
) (*Session, error) {
	var sess Session

	err := s.read.QueryRowContext(ctx,
		`SELECT id, user_id, token_hash, expires_at, created_at
		 FROM sessions WHERE token_hash = ?`, tokenHash,
	).Scan(&sess.ID, &sess.UserID, &sess.TokenHash,
		&sess.ExpiresAt, &sess.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("getting session by hash: %w", err)
	}

	return &sess, nil
}

// DeleteSession removes a session by ID.
func (s *Store) DeleteSession(ctx context.Context, id int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM sessions WHERE id = ?", id,
	)
	if err != nil {
		return fmt.Errorf("deleting session %d: %w", id, err)
	}

	return nil
}

// DeleteUserSessions removes all sessions for a user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.write.ExecContext(ctx,
		"DELETE FROM sessions WHERE user_id = ?", userID,
	)
	if err != nil {
		return fmt.Errorf("deleting sessions for user %d: %w", userID, err)
	}

	return nil
}

// CleanupExpiredSessions deletes sessions past their expiration.
func (s *Store) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	result, err := s.write.ExecContext(ctx,
		"DELETE FROM sessions WHERE expires_at < CURRENT_TIMESTAMP",
	)
	if err != nil {
		return 0, fmt.Errorf("cleaning up expired sessions: %w", err)
	}

	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("getting session cleanup count: %w", err)
	}

	return n, nil
}

// RecordLoginAttempt notes a magic-link request for a (hashed) email
// address — known or unknown alike, so rate-limit behaviour can't be used
// to probe which addresses have accounts.
func (s *Store) RecordLoginAttempt(ctx context.Context, emailHash string) error {
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO login_attempts (email_hash) VALUES (?)`, emailHash)
	if err != nil {
		return fmt.Errorf("recording login attempt: %w", err)
	}

	return nil
}

// CountRecentLoginAttempts counts magic-link requests for a (hashed) email
// address in the last hour.
func (s *Store) CountRecentLoginAttempts(
	ctx context.Context, emailHash string,
) (int, error) {
	var count int

	err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM login_attempts
		 WHERE email_hash = ? AND created_at > datetime('now', '-1 hour')`,
		emailHash,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting recent login attempts: %w", err)
	}

	return count, nil
}
