-- Issue-level "photo & video dump": loose media members attach to a round,
-- independent of any question. Rendered as the collage closer page of the
-- published issue.
CREATE TABLE dump_items (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('photo', 'video')),
    content_type TEXT,
    file_path TEXT NOT NULL,
    caption TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_dump_items_issue_user_sort
    ON dump_items(issue_id, user_id, sort_order);
