-- 016: multi-group ("many Loops, one instance").
--
-- Introduces groups, per-group memberships (roles move off users), per-group
-- settings rows, and instance-level settings. All existing data becomes
-- group 1. On a fresh database the old tables are empty, no group row is
-- created here, and startup seeding creates group 1 + its settings so the
-- out-of-box single-Loop experience is unchanged.
--
-- Several tables gain a NOT NULL group_id with a REFERENCES clause, and
-- users drops its role column — SQLite requires full table rebuilds for
-- both. users/settings/issues/question_bank/default_questions are referenced
-- by other tables, so the rebuilds need foreign keys off for the
-- DROP/RENAME (migrate:fk_off) — the runner verifies with
-- foreign_key_check before committing.

CREATE TABLE groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Existing installs (settings row or users present) become group 1.
INSERT INTO groups (id)
SELECT 1
WHERE EXISTS (SELECT 1 FROM settings) OR EXISTS (SELECT 1 FROM users);

-- Fresh databases run every migration in one go: 011 seeded default
-- questions before any group can exist. Drop such orphan rows — startup
-- group creation re-seeds them per group — so the group_id backfill below
-- never leaves dangling references on a brand-new database.
DELETE FROM default_questions
WHERE NOT EXISTS (SELECT 1 FROM groups WHERE id = 1);

DELETE FROM question_bank
WHERE NOT EXISTS (SELECT 1 FROM groups WHERE id = 1);

CREATE TABLE memberships (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id INTEGER NOT NULL REFERENCES groups(id),
    user_id INTEGER NOT NULL REFERENCES users(id),
    role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('admin', 'member')),
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(group_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_memberships_user ON memberships(user_id);
CREATE INDEX IF NOT EXISTS idx_memberships_group ON memberships(group_id);

INSERT INTO memberships (group_id, user_id, role, is_active)
SELECT 1, id, role, is_active FROM users;

-- Instance-level settings: operator branding plus policy ceilings that
-- individual Loops opt in underneath (allow_public_mementos is ANDed with
-- each group's own setting). Seeded from group 1's settings when present.
CREATE TABLE instance_settings (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    instance_name TEXT NOT NULL DEFAULT 'PiecesOfLife',
    allow_public_mementos BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO instance_settings (id, instance_name, allow_public_mementos)
SELECT 1, loop_name, allow_public_mementos FROM settings WHERE id = 1;

INSERT OR IGNORE INTO instance_settings (id) VALUES (1);

-- users: role moves to memberships; instance admins are flagged on the
-- identity; last_group_id remembers where a fresh login should land.
CREATE TABLE users_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    avatar_url TEXT,
    bio TEXT,
    is_active BOOLEAN NOT NULL DEFAULT 1,
    is_instance_admin BOOLEAN NOT NULL DEFAULT 0,
    last_group_id INTEGER REFERENCES groups(id),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO users_new
    (id, name, email, avatar_url, bio, is_active, is_instance_admin,
     last_group_id, created_at)
SELECT id, name, email, avatar_url, bio, is_active,
       CASE WHEN role = 'admin' THEN 1 ELSE 0 END,
       1, created_at
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

-- settings: one row per group instead of a single-row table. Column set is
-- unchanged; the CHECK(id = 1) is dropped and group_id added.
CREATE TABLE settings_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id INTEGER NOT NULL UNIQUE REFERENCES groups(id),
    loop_name TEXT NOT NULL DEFAULT 'PiecesOfLife',
    tagline TEXT,
    frequency TEXT NOT NULL DEFAULT 'monthly' CHECK(frequency IN ('biweekly', 'monthly', 'quarterly')),
    submission_window_days INTEGER NOT NULL DEFAULT 7,
    start_datetime DATETIME,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    invite_note TEXT,
    setup_complete BOOLEAN NOT NULL DEFAULT 0,
    accent_color TEXT NOT NULL DEFAULT '#2d5016',
    auto_create_enabled BOOLEAN NOT NULL DEFAULT 1,
    allow_public_mementos BOOLEAN NOT NULL DEFAULT 1,
    questions_per_issue INTEGER NOT NULL DEFAULT 6,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO settings_new
    (id, group_id, loop_name, tagline, frequency, submission_window_days,
     start_datetime, timezone, invite_note, setup_complete, accent_color,
     auto_create_enabled, allow_public_mementos, questions_per_issue,
     created_at, updated_at)
SELECT id, 1, loop_name, tagline, frequency, submission_window_days,
       start_datetime, timezone, invite_note, setup_complete, accent_color,
       auto_create_enabled, allow_public_mementos, questions_per_issue,
       created_at, updated_at
FROM settings;

DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;

-- issues: the Loop everything below an issue (questions, responses, blocks,
-- comments, dump items) inherits through issue_id.
CREATE TABLE issues_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id INTEGER NOT NULL REFERENCES groups(id),
    title TEXT,
    month INTEGER NOT NULL,
    year INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'collecting', 'published')),
    opens_at DATETIME NOT NULL,
    deadline DATETIME NOT NULL,
    published_at DATETIME,
    count_admin_in BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO issues_new
    (id, group_id, title, month, year, status, opens_at, deadline,
     published_at, count_admin_in, created_at)
SELECT id, 1, title, month, year, status, opens_at, deadline,
       published_at, count_admin_in, created_at
FROM issues;

DROP TABLE issues;
ALTER TABLE issues_new RENAME TO issues;

CREATE INDEX IF NOT EXISTS idx_issues_group_status ON issues(group_id, status);

-- question_bank: the used flag is per-Loop, and every Loop gets its own copy
-- of the seed questions (SeedQuestionBank tops up per group).
CREATE TABLE question_bank_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id INTEGER NOT NULL REFERENCES groups(id),
    text TEXT NOT NULL,
    category TEXT NOT NULL,
    used BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(group_id, text)
);

INSERT INTO question_bank_new (id, group_id, text, category, used, created_at)
SELECT id, 1, text, category, used, created_at FROM question_bank;

DROP TABLE question_bank;
ALTER TABLE question_bank_new RENAME TO question_bank;

CREATE INDEX IF NOT EXISTS idx_question_bank_group_used
    ON question_bank(group_id, used);

-- default_questions: per-Loop default prompts.
CREATE TABLE default_questions_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id INTEGER NOT NULL REFERENCES groups(id),
    text TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT 1,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(group_id, text)
);

INSERT INTO default_questions_new
    (id, group_id, text, enabled, sort_order, created_at)
SELECT id, 1, text, enabled, sort_order, created_at FROM default_questions;

DROP TABLE default_questions;
ALTER TABLE default_questions_new RENAME TO default_questions;

CREATE INDEX IF NOT EXISTS idx_default_questions_group_sort
    ON default_questions(group_id, sort_order);

-- sessions carry the user's current Loop; email_log rows are tagged with
-- their Loop so the admin email log stays per-Loop. Plain column adds:
-- nullable with NULL defaults, so no rebuild needed.
ALTER TABLE sessions ADD COLUMN group_id INTEGER REFERENCES groups(id);
ALTER TABLE email_log ADD COLUMN group_id INTEGER REFERENCES groups(id);

UPDATE sessions SET group_id = 1
WHERE EXISTS (SELECT 1 FROM groups WHERE id = 1);

UPDATE email_log SET group_id = 1
WHERE EXISTS (SELECT 1 FROM groups WHERE id = 1);

CREATE INDEX IF NOT EXISTS idx_email_log_group_created
    ON email_log(group_id, created_at);

-- migrate:fk_off
