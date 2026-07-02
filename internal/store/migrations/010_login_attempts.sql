-- Rate-limit magic-link requests per email address rather than per user
-- account. Keying by (hashed) address means unknown emails accumulate
-- attempts exactly like real ones, so the login endpoint's responses are
-- indistinguishable between the two — no user enumeration via the
-- rate-limit boundary.
CREATE TABLE login_attempts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email_hash TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_login_attempts_hash_time
    ON login_attempts(email_hash, created_at);
