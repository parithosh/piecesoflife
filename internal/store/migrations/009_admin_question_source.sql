-- Allow 'admin' as a question source: admins can add and edit questions on
-- the live round from the dashboard. The original CHECK only permitted
-- bank/friend, which made POST /api/issues/{id}/questions fail outright.
-- SQLite cannot alter a CHECK in place, so recreate the table. responses
-- references questions, so the rebuild needs foreign keys off for the
-- DROP/RENAME (migrate:fk_off) — the runner verifies with
-- foreign_key_check before committing.
CREATE TABLE questions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id),
    text TEXT NOT NULL,
    category TEXT,
    source TEXT NOT NULL DEFAULT 'bank' CHECK(source IN ('bank', 'friend', 'admin')),
    submitted_by INTEGER REFERENCES users(id),
    sort_order INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO questions_new
    (id, issue_id, text, category, source, submitted_by, sort_order, created_at)
SELECT id, issue_id, text, category, source, submitted_by, sort_order, created_at
FROM questions;

DROP TABLE questions;
ALTER TABLE questions_new RENAME TO questions;

CREATE INDEX IF NOT EXISTS idx_questions_issue_sort
    ON questions(issue_id, sort_order);
