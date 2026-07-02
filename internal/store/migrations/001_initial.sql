-- Users are never hard-deleted. Deactivation is via is_active = false.
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    avatar_url TEXT,
    bio TEXT,
    role TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('admin', 'member')),
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Auth tokens: supports multiple concurrent tokens per user (login + email CTA)
CREATE TABLE auth_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    token_hash TEXT NOT NULL UNIQUE,
    type TEXT NOT NULL CHECK(type IN ('login', 'email_cta')),
    expires_at DATETIME NOT NULL,
    consumed_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Server-side sessions: supports revocation and survives restarts
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    token_hash TEXT NOT NULL UNIQUE,
    expires_at DATETIME NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- App settings (single-row table, populated by onboarding wizard)
CREATE TABLE settings (
    id INTEGER PRIMARY KEY CHECK(id = 1),
    loop_name TEXT NOT NULL DEFAULT 'PiecesOfLife',
    tagline TEXT,
    frequency TEXT NOT NULL DEFAULT 'monthly' CHECK(frequency IN ('biweekly', 'monthly', 'quarterly')),
    submission_window_days INTEGER NOT NULL DEFAULT 7,
    start_datetime DATETIME,
    timezone TEXT NOT NULL DEFAULT 'UTC',
    invite_note TEXT,
    setup_complete BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT,
    month INTEGER NOT NULL,
    year INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'collecting', 'published')),
    opens_at DATETIME NOT NULL,
    deadline DATETIME NOT NULL,
    published_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id),
    text TEXT NOT NULL,
    category TEXT,
    source TEXT NOT NULL DEFAULT 'bank' CHECK(source IN ('bank', 'friend')),
    submitted_by INTEGER REFERENCES users(id),
    sort_order INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Note: issue_id is intentionally omitted — derive via question_id → questions.issue_id.
CREATE TABLE responses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    question_id INTEGER NOT NULL REFERENCES questions(id),
    is_draft BOOLEAN NOT NULL DEFAULT 1,
    version INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, question_id)
);

-- Block-based response content: ordered text, media, and link blocks
CREATE TABLE response_blocks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    response_id INTEGER NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK(type IN ('text', 'photo', 'audio', 'video', 'link')),
    content TEXT,
    file_path TEXT,
    caption TEXT,
    link_url TEXT,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE comments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    response_id INTEGER NOT NULL REFERENCES responses(id),
    parent_id INTEGER REFERENCES comments(id),
    body TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Pre-generated question bank
CREATE TABLE question_bank (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL,
    used BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE email_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER REFERENCES users(id),
    issue_id INTEGER REFERENCES issues(id),
    type TEXT NOT NULL CHECK(type IN ('invite', 'open', 'reminder', 'published', 'comment_notification')),
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'sent', 'failed')),
    sent_at DATETIME,
    error TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Per-user notification preferences
CREATE TABLE notification_preferences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL UNIQUE REFERENCES users(id),
    issue_open BOOLEAN NOT NULL DEFAULT 1,
    reminders BOOLEAN NOT NULL DEFAULT 1,
    published BOOLEAN NOT NULL DEFAULT 1,
    comment_notify BOOLEAN NOT NULL DEFAULT 1,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Tracks scheduler events to prevent re-execution after restarts
CREATE TABLE scheduler_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER REFERENCES issues(id),
    event_type TEXT NOT NULL CHECK(event_type IN ('reminder_1', 'reminder_2', 'auto_close', 'token_cleanup', 'session_cleanup')),
    scheduled_at DATETIME NOT NULL,
    fired_at DATETIME,
    was_late BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id, event_type, scheduled_at)
);
