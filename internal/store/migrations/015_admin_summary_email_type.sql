-- admin_summary: a pre-publish roster email to the admin (who has answered,
-- who hasn't), queued the day before the deadline. Two CHECK constraints
-- must widen, and SQLite can't alter a CHECK in place:
--
--   - scheduler_events.event_type gains 'admin_summary'. email_log
--     references scheduler_events, so this rebuild needs foreign keys off
--     for the DROP/RENAME (migrate:fk_off) — the runner verifies with
--     foreign_key_check before committing.
--   - email_log.type gains 'admin_summary', plus 'test' (the settings-page
--     test send, which the old CHECK silently rejected from logging).
--     Nothing references email_log, so its rebuild is plain.

CREATE TABLE scheduler_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER REFERENCES issues(id),
    event_type TEXT NOT NULL CHECK(event_type IN (
        'reminder_1', 'reminder_2', 'auto_close',
        'token_cleanup', 'session_cleanup',
        'create_next_issue', 'admin_summary'
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

CREATE INDEX IF NOT EXISTS idx_scheduler_events_pending
    ON scheduler_events(fired_at, scheduled_at);

CREATE TABLE email_log_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER REFERENCES users(id),
    issue_id INTEGER REFERENCES issues(id),
    type TEXT NOT NULL CHECK(type IN (
        'invite', 'open', 'reminder', 'published',
        'comment_notification', 'admin_summary', 'test'
    )),
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'sent', 'failed')),
    sent_at DATETIME,
    error TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    scheduler_event_id INTEGER REFERENCES scheduler_events(id)
);

INSERT INTO email_log_new
    (id, user_id, issue_id, type, status, sent_at, error, created_at, scheduler_event_id)
SELECT id, user_id, issue_id, type, status, sent_at, error, created_at, scheduler_event_id
FROM email_log;

DROP TABLE email_log;
ALTER TABLE email_log_new RENAME TO email_log;

CREATE UNIQUE INDEX IF NOT EXISTS idx_email_log_scheduler_event_user_type
    ON email_log(scheduler_event_id, user_id, type)
    WHERE scheduler_event_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_email_log_created
    ON email_log(created_at);
