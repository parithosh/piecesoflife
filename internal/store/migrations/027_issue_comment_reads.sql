-- 027: per-member read watermark for unseen comments on a published issue.
-- Tracks both the timestamp and highest comment ID displayed so comments
-- arriving in the same second or between page load and tab click are never
-- silently marked seen.

CREATE TABLE IF NOT EXISTS issue_comment_reads (
    user_id              INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issue_id             INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    last_seen_at         DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_comment_id INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, issue_id)
);
