-- 021: comments on individual notebook (diary) blocks.
--
-- Rambles are now written as separate blocks, and each block published in a
-- notebook spread carries its own thread. comments gains a fourth target
-- (diary_block_id); existing diary_day_id comments are left where they are —
-- days published before this migration keep their day-level threads, new
-- conversations start on blocks.
--
-- Same rebuild dance as 019: SQLite can't widen a CHECK in place, and
-- comments is referenced by itself and comment_notifications, so the
-- DROP/RENAME runs with foreign keys off (migrate:fk_off).

CREATE TABLE comments_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    response_id INTEGER REFERENCES responses(id),
    diary_day_id INTEGER REFERENCES diary_days(id) ON DELETE CASCADE,
    diary_block_id INTEGER REFERENCES diary_blocks(id) ON DELETE CASCADE,
    dump_item_id INTEGER REFERENCES dump_items(id) ON DELETE CASCADE,
    parent_id INTEGER REFERENCES comments(id),
    body TEXT NOT NULL,
    edited_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    CHECK(
        (response_id IS NOT NULL) + (diary_day_id IS NOT NULL) +
        (diary_block_id IS NOT NULL) + (dump_item_id IS NOT NULL) = 1
    )
);

INSERT INTO comments_new
    (id, user_id, response_id, diary_day_id, dump_item_id, parent_id,
     body, edited_at, created_at)
SELECT id, user_id, response_id, diary_day_id, dump_item_id, parent_id,
       body, edited_at, created_at
FROM comments;

DROP TABLE comments;
ALTER TABLE comments_new RENAME TO comments;

CREATE INDEX IF NOT EXISTS idx_comments_response_created
    ON comments(response_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_diary_day_created
    ON comments(diary_day_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_diary_block_created
    ON comments(diary_block_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_dump_item_created
    ON comments(dump_item_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_parent
    ON comments(parent_id);
