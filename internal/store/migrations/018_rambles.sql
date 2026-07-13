-- 018: Ramble — private daily notes, plus per-issue "From the notebooks"
-- diary sections.
--
-- rambles/ramble_blocks are the PRIVATE journal: person-scoped (no
-- group_id — one journal per person across every Loop), one page per
-- calendar day. `day` is a local 'YYYY-MM-DD' string chosen by the client
-- when the page was opened; it is a label, not a timestamp.
--
-- diary_sections/diary_days/diary_blocks are SNAPSHOT COPIES a member
-- explicitly attaches to one issue. Editing a snapshot never touches the
-- journal, and nothing flows from the journal into an issue except through
-- an attach/refresh. Uploaded files are shared by path between a ramble
-- block and its diary copies — deletion paths check for remaining
-- references before unlinking (see server upload cleanup).
--
-- comments gains an optional diary-day target so published notebook days
-- carry the same threads answers do. SQLite can't relax NOT NULL in place,
-- so comments is rebuilt (migrate:fk_off).

CREATE TABLE rambles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, day)
);

CREATE INDEX IF NOT EXISTS idx_rambles_user_day ON rambles(user_id, day);

CREATE TABLE ramble_blocks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ramble_id INTEGER NOT NULL REFERENCES rambles(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK(type IN ('text', 'photo', 'audio', 'video')),
    content TEXT,
    file_path TEXT,
    caption TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_ramble_blocks_ramble_sort
    ON ramble_blocks(ramble_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_ramble_blocks_file_path
    ON ramble_blocks(file_path);

-- One diary section per (issue, member). copied_through is the last journal
-- day already offered to this section — refresh copies days from it
-- (inclusive, so a page that grows the same day it was attached stays
-- pullable) that the section doesn't already hold. A trimmed day earlier
-- than copied_through never reappears.
CREATE TABLE diary_sections (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id INTEGER NOT NULL REFERENCES users(id),
    copied_through TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_diary_sections_issue ON diary_sections(issue_id);

CREATE TABLE diary_days (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    section_id INTEGER NOT NULL REFERENCES diary_sections(id) ON DELETE CASCADE,
    day TEXT NOT NULL CHECK(day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(section_id, day)
);

CREATE INDEX IF NOT EXISTS idx_diary_days_section_day
    ON diary_days(section_id, day);

CREATE TABLE diary_blocks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    diary_day_id INTEGER NOT NULL REFERENCES diary_days(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK(type IN ('text', 'photo', 'audio', 'video')),
    content TEXT,
    file_path TEXT,
    caption TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_diary_blocks_day_sort
    ON diary_blocks(diary_day_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_diary_blocks_file_path
    ON diary_blocks(file_path);

-- comments: response_id relaxes to nullable and diary_day_id joins it —
-- exactly one target must be set. Full rebuild; comments is referenced only
-- by itself (parent_id), but the DROP/RENAME still needs foreign keys off
-- (migrate:fk_off) so the self-reference doesn't trip the swap.
CREATE TABLE comments_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    response_id INTEGER REFERENCES responses(id),
    diary_day_id INTEGER REFERENCES diary_days(id) ON DELETE CASCADE,
    parent_id INTEGER REFERENCES comments(id),
    body TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    CHECK((response_id IS NULL) != (diary_day_id IS NULL))
);

INSERT INTO comments_new (id, user_id, response_id, parent_id, body, created_at)
SELECT id, user_id, response_id, parent_id, body, created_at FROM comments;

DROP TABLE comments;
ALTER TABLE comments_new RENAME TO comments;

CREATE INDEX IF NOT EXISTS idx_comments_response_created
    ON comments(response_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_diary_day_created
    ON comments(diary_day_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_parent
    ON comments(parent_id);
