-- 027: per-member read watermark for unseen comments on a published issue.
-- Used by the "Unseen comments" tab to surface only comment activity from the
-- last 2 days that the member has not yet caught up on.

CREATE TABLE IF NOT EXISTS issue_comment_reads (
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issue_id     INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    last_seen_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, issue_id)
);
