-- 028: per-member likes on published answers (responses) and comments.
-- Exactly one of response_id or comment_id is set per row.

CREATE TABLE IF NOT EXISTS likes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    response_id INTEGER REFERENCES responses(id) ON DELETE CASCADE,
    comment_id  INTEGER REFERENCES comments(id) ON DELETE CASCADE,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK ((response_id IS NOT NULL) + (comment_id IS NOT NULL) = 1)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_likes_user_response
    ON likes(user_id, response_id) WHERE response_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_likes_user_comment
    ON likes(user_id, comment_id) WHERE comment_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_likes_response
    ON likes(response_id) WHERE response_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_likes_comment
    ON likes(comment_id) WHERE comment_id IS NOT NULL;
