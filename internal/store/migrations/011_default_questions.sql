-- Default questions: a small set of prompts stitched into every issue on top
-- of the bank/friend questions. The admin can disable each one globally here
-- (enabled flag) and still edit or remove the copy landed on a specific
-- issue, since they become ordinary questions rows with source 'default'.
--
-- questions.source gains that 'default' value, and SQLite cannot alter a
-- CHECK in place, so the table is rebuilt. responses references questions,
-- so the rebuild needs foreign keys off for the DROP/RENAME
-- (migrate:fk_off) — the runner verifies with foreign_key_check before
-- committing.
CREATE TABLE questions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id),
    text TEXT NOT NULL,
    category TEXT,
    source TEXT NOT NULL DEFAULT 'bank' CHECK(source IN ('bank', 'friend', 'admin', 'default')),
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

CREATE TABLE default_questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL UNIQUE,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO default_questions (text, sort_order) VALUES
    ('What good thing happened this month?', 0),
    ('What bad thing happened this month?', 1),
    ('Free space for random thoughts', 2);

-- How many questions each issue aims for from friend suggestions + the bank
-- (default questions ride on top of this number).
ALTER TABLE settings ADD COLUMN questions_per_issue INTEGER NOT NULL DEFAULT 6;
