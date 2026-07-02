-- 002: theming + auto-create + mementos
-- Adds settings columns driving the Phase 3/4 features introduced alongside
-- this migration. All columns have sensible defaults so existing rows keep
-- working without backfill.

ALTER TABLE settings ADD COLUMN accent_color TEXT NOT NULL DEFAULT '#2d5016';
ALTER TABLE settings ADD COLUMN auto_create_enabled BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE settings ADD COLUMN allow_public_mementos BOOLEAN NOT NULL DEFAULT 1;

-- create_next_issue is a new scheduler event fired after an issue publishes
-- when auto_create_enabled is true. Add to the CHECK set by recreating the
-- table (SQLite can't modify CHECK in place). We keep the same column set
-- and data; only the CHECK constraint changes.

CREATE TABLE scheduler_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER REFERENCES issues(id),
    event_type TEXT NOT NULL CHECK(event_type IN (
        'reminder_1', 'reminder_2', 'auto_close',
        'token_cleanup', 'session_cleanup',
        'create_next_issue'
    )),
    scheduled_at DATETIME NOT NULL,
    fired_at DATETIME,
    was_late BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id, event_type, scheduled_at)
);

INSERT INTO scheduler_events_new
    (id, issue_id, event_type, scheduled_at, fired_at, was_late, created_at)
SELECT id, issue_id, event_type, scheduled_at, fired_at, was_late, created_at
FROM scheduler_events;

DROP TABLE scheduler_events;
ALTER TABLE scheduler_events_new RENAME TO scheduler_events;
