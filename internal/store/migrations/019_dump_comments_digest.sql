-- 019: comments on dump items, comment editing, and daily digest emails.
--
-- comments gains a third target (dump_item_id — the photo & video dump
-- collage) and an edited_at stamp. SQLite can't widen a CHECK in place, so
-- comments is rebuilt; it is referenced only by itself and by
-- comment_notifications (created below), so the DROP/RENAME runs with
-- foreign keys off (migrate:fk_off).
--
-- comment_notifications queues "someone commented on your content" emails:
-- one row per (recipient, comment), drained once a day by the
-- comment_digest scheduler event into a single email per recipient. A
-- deleted comment takes its pending notification with it (CASCADE), and the
-- digest reads bodies at send time, so edits are reflected.
--
-- scheduler_events.event_type gains 'comment_digest' — same rebuild dance
-- as 015 (email_log references scheduler_events, hence fk_off again).

CREATE TABLE comments_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    response_id INTEGER REFERENCES responses(id),
    diary_day_id INTEGER REFERENCES diary_days(id) ON DELETE CASCADE,
    dump_item_id INTEGER REFERENCES dump_items(id) ON DELETE CASCADE,
    parent_id INTEGER REFERENCES comments(id),
    body TEXT NOT NULL,
    edited_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    CHECK(
        (response_id IS NOT NULL) + (diary_day_id IS NOT NULL) +
        (dump_item_id IS NOT NULL) = 1
    )
);

INSERT INTO comments_new
    (id, user_id, response_id, diary_day_id, parent_id, body, created_at)
SELECT id, user_id, response_id, diary_day_id, parent_id, body, created_at
FROM comments;

DROP TABLE comments;
ALTER TABLE comments_new RENAME TO comments;

CREATE INDEX IF NOT EXISTS idx_comments_response_created
    ON comments(response_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_diary_day_created
    ON comments(diary_day_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_dump_item_created
    ON comments(dump_item_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_parent
    ON comments(parent_id);

CREATE TABLE comment_notifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    recipient_id INTEGER NOT NULL REFERENCES users(id),
    comment_id INTEGER NOT NULL REFERENCES comments(id) ON DELETE CASCADE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(recipient_id, comment_id)
);

CREATE INDEX IF NOT EXISTS idx_comment_notifications_recipient
    ON comment_notifications(recipient_id);

CREATE TABLE scheduler_events_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER REFERENCES issues(id),
    event_type TEXT NOT NULL CHECK(event_type IN (
        'reminder_1', 'reminder_2', 'auto_close',
        'token_cleanup', 'session_cleanup',
        'create_next_issue', 'admin_summary', 'comment_digest'
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
