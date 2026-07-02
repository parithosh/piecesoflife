# PiecesOfLife — Technical Specification (v4)

> A self-hosted, private group newsletter platform for friends.
> Inspired by a similar service. Built for privacy, control, and genuine connection.
>
> **v4 changes:** Go monolith with oat.ink UI, merged scheduler, Fastmail SMTP,
> pre-generated question bank, auto-approved friend questions, no photo compression,
> plain filesystem storage, Go-native email templates (no MJML), separate auth_tokens
> table, sessions table, settings table, UTC-everywhere, graceful shutdown, health
> endpoint, upload limits (50MB/file, 10 files/response), optimistic concurrency on
> autosave, scheduler missed-event recovery, structured logging.

---

## 1. Product Overview

### What It Does
PiecesOfLife is a monthly ritual where your friend group answers thoughtful, unique questions about their lives. Each month:
1. **Questions go out** — a mix of pre-curated questions and friend-submitted ones (auto-approved)
2. **Friends respond** — via a hosted web page with text answers + photo uploads
3. **Reminders nudge** — automated emails at key deadlines
4. **The issue drops** — a beautifully compiled newsletter of everyone's answers, delivered by email and viewable on the web
5. **Friends react** — comments and emoji reactions on each answer

### Core Principles
- **Self-hosted** — runs on your homelab, data never leaves your infrastructure
- **Email-first** — all key interactions trigger email; the web UI is the companion
- **Low-friction** — friends shouldn't need to install anything or create complex accounts
- **Unique questions** — pre-generated question bank + friend-submitted questions, refreshed monthly
- **Delightful UX** — guided flows, celebratory feedback, and clear signifiers at every step
- **Dead simple ops** — single Go binary, SQLite, filesystem uploads, one Docker container

---

## 2. Architecture Overview

### High-Level Stack

PiecesOfLife is a **Go monolith** — a single binary that serves the web UI, handles API
requests, runs the background scheduler, and sends emails. No separate processes,
no JS build pipeline, no container orchestration.

```
┌─────────────────────────────────────────────────────┐
│                    Traefik (Reverse Proxy)           │
│                 piecesoflife.yourdomain.com            │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│         PiecesOfLife (single Go binary)                │
│                                                      │
│  ┌────────────────────────────────────────────────┐  │
│  │  HTTP Server (net/http)                        │  │
│  │                                                │  │
│  │  ┌──────────────┐  ┌───────────────────────┐   │  │
│  │  │  Page Routes  │  │  API Routes (/api/*)  │   │  │
│  │  │  html/template│  │  JSON handlers        │   │  │
│  │  │  + oat.ink    │  │                       │   │  │
│  │  └──────────────┘  └───────────────────────┘   │  │
│  └────────────────────────────────────────────────┘  │
│                                                      │
│  ┌────────────────────────────────────────────────┐  │
│  │  Background Scheduler (goroutine)              │  │
│  │  • Reminder emails (day 7, day 12)             │  │
│  │  • Auto-publish on deadline                    │  │
│  │  • Reaction digest (daily)                     │  │
│  │  • Token cleanup (expired magic links)         │  │
│  └────────────────────────────────────────────────┘  │
│                                                      │
│  ┌──────────┐  ┌──────────┐  ┌──────────────────┐   │
│  │  SQLite   │  │  SMTP    │  │  Filesystem      │   │
│  │  (data)   │  │ (Fastmail│  │  /data/uploads   │   │
│  │           │  │  :465)   │  │  (photos)        │   │
│  └──────────┘  └──────────┘  └──────────────────┘   │
└──────────────────────────────────────────────────────┘
```

### Technology Choices

| Component | Choice | Rationale |
|-----------|--------|-----------|
| **Language** | **Go** | Single binary, low memory (~50MB), goroutines for scheduler, `html/template` for pages. |
| **UI Library** | **oat.ink** | ~8KB CSS+JS, semantic HTML, zero dependencies, no build step. Vendored into the binary. |
| **Web Framework** | **net/http** (stdlib, Go 1.22+) | Built-in pattern matching is sufficient. No heavy framework needed. |
| **Database** | **SQLite** (via `modernc.org/sqlite`) | Pure Go, no CGO. Zero-ops, single file. PRAGMAs set on connection open: `journal_mode=WAL` (concurrent reads), `foreign_keys=ON` (SQLite defaults to OFF), `busy_timeout=5000` (wait on lock instead of failing), `synchronous=NORMAL` (safe with WAL, better perf). |
| **Migrations** | **Embedded SQL files** (`go:embed`) | Numbered files (`001_initial.sql`, `002_add_invite_note.sql`, etc.). Applied on startup via a `schema_migrations` table. No rollback — fix forward. Use `goose` or ~50 lines of custom code. |
| **Email** | **Fastmail SMTP** (`smtp.fastmail.com:465`) | Your existing custom domain + app-specific password. DKIM/SPF already configured. |
| **Auth** | **Magic link** (email-based) | Zero password management. Friends click a link → logged in. |
| **Scheduler** | **Goroutine + time.Ticker** or **go-co-op/gocron** | Runs inside the same process. No external cron or separate container. |
| **File Storage** | **Local filesystem** (Docker volume at `/data/uploads`) | Simple, fast, easy to backup with rsync/rclone. No S3 overhead. Path convention: `/data/uploads/{year}/{month}/{random_id}_{filename}`. Random ID prefix prevents collisions. DB stores relative path (`/uploads/2026/03/abc123_summit.jpg`), served at same URL path via authenticated `/uploads/*` route. |
| **Email Templates** | **Go `html/template`** with inline CSS | Go-native approach. No MJML, no Node.js build step. Hand-crafted responsive HTML embedded in the binary. |
| **Templates** | **Go `html/template`** + **oat.ink CSS/JS** | Server-rendered HTML. Pages are fast, cacheable, and work without JS (progressive enhancement for the editor). |
| **Containerization** | **Docker Compose** (single service) | Fits your existing Traefik + Komodo GitOps setup. |

### Why Go + oat.ink Monolith?

- **Single binary**: `go build` → one file → one Docker container. No Node.js, no `npm install`, no webpack.
- **Server-rendered pages**: Go's `html/template` renders pages. oat.ink provides semantic styling (buttons, cards, forms, dialogs, tabs) with no CSS framework.
- **Progressive enhancement**: Pages work without JS. The block-based editor adds interactivity via a small vanilla JS module (~200 lines) for drag-and-drop, auto-save, and block insertion using fetch + DOM manipulation.
- **Embedded assets**: oat.ink CSS/JS and email templates are embedded in the binary with `go:embed`. Zero external file dependencies at runtime. **Dev mode**: when `DEV_MODE=true` env var is set, read templates and static files from the filesystem instead of embedded copies, enabling hot-reload without rebuilding.
- **Goroutine scheduler**: Background jobs (reminders, auto-publish, cleanup) run as a goroutine inside `main()`. No separate process, no message queue, no Redis.

---

## 3. Data Model

### Entity Relationship

```
┌─────────────┐     ┌──────────────┐     ┌──────────────┐
│    User      │────<│  Response     │>────│   Question   │
│              │     │              │     │              │
│ id           │     │ id           │     │ id           │
│ name         │     │ user_id  (FK)│     │ text         │
│ email        │     │ question_id  │     │ issue_id (FK)│
│ avatar_url   │     │ is_draft     │     │ category     │
│ bio          │     │ version      │     │ source (bank/│
│ role (admin/ │     │ created_at   │     │  friend)     │
│   member)    │     │ updated_at   │     │ submitted_by │
│ is_active    │     └──────┬───────┘     │ created_at   │
│ created_at   │            │             └──────────────┘
└─────────────┘     ┌──────▼───────┐
       │            │ ResponseBlock│  (ordered content blocks)
       │            │              │
       │            │ id           │
       │            │ response_id  │
       │            │   (FK)       │
       │            │ type (text/  │
       │            │  photo/link) │
       │            │ content      │  ← text content or display text
       │            │ file_path    │  ← for photo blocks
       │            │ caption      │  ← for photo blocks
       │            │ link_url     │  ← for link blocks (Spotify etc)
       │            │ sort_order   │
       │            │ created_at   │
       │            │ updated_at   │
       │            └──────────────┘
       │
       │     ┌──────────────┐     ┌──────────────┐
       │────<│   Comment    │     │    Issue      │
       │     │              │     │              │
       │     │ id           │     │ id           │
       │     │ user_id (FK) │     │ title        │
       │     │ response_id  │     │ month (int)  │
       │     │   (FK)       │     │ year (int)   │
       │     │ parent_id    │     │ status (draft│
       │     │   (FK, self) │     │  /collecting │
       │     │ body         │     │  /published) │
       │     │ created_at   │     │ opens_at     │
       │     └──────────────┘     │ deadline     │
       │                          │ published_at │
       │     ┌──────────────┐     │ created_at   │
       └────<│  Reaction    │     └──────────────┘
             │              │
             │ id           │
             │ user_id (FK) │
             │ response_id  │
             │   (FK)       │
             │ emoji        │
             │ created_at   │
             └──────────────┘

┌──────────────┐     ┌──────────────┐     ┌──────────────────┐
│ QuestionBank │     │  EmailLog    │     │ NotificationPref │
│              │     │              │     │                  │
│ id           │     │ id           │     │ id               │
│ text         │     │ user_id (FK) │     │ user_id (FK)     │
│ category     │     │ issue_id (FK)│     │ issue_open       │
│ used (bool)  │     │ type         │     │ reminders        │
│ created_at   │     │ sent_at      │     │ published        │
└──────────────┘     │ status       │     │ comment_notify   │
                     └──────────────┘     │ reaction_notify  │
                                          │ updated_at       │
                                          └──────────────────┘

┌──────────────┐     ┌──────────────┐     ┌──────────────────┐
│  AuthToken   │     │   Session    │     │    Settings       │
│              │     │              │     │ (single row)      │
│ id           │     │ id           │     │                   │
│ user_id (FK) │     │ user_id (FK) │     │ id                │
│ token_hash   │     │ token_hash   │     │ loop_name         │
│ type (login/ │     │ expires_at   │     │ tagline           │
│  email_cta)  │     │ created_at   │     │ frequency         │
│ expires_at   │     └──────────────┘     │ submission_window │
│ consumed_at  │                          │ start_datetime    │
│ created_at   │                          │ timezone          │
└──────────────┘                          │ invite_note       │
                                          │ setup_complete    │
                                          │ created_at        │
                                          │ updated_at        │
                                          └───────────────────┘

┌────────────────────┐
│  SchedulerEvent    │  (tracks fired events to prevent re-execution)
│                    │
│ id                 │
│ issue_id (FK)      │
│ event_type         │  ← reminder_1, reminder_2, auto_close, auto_publish, token_cleanup, etc.
│                    │  UNIQUE(issue_id, event_type, scheduled_at)
│ scheduled_at       │
│ fired_at           │
│ was_late (bool)    │  ← true if fired after a restart catch-up
│ created_at         │
└────────────────────┘
```

### Key Design Decision: Block-Based Responses

Instead of a flat `body` text field with separate photo attachments, responses use an
**ordered block model**. This lets friends interleave text and photos in whatever order
tells their story best — a key improvement over a similar service, which only supports one
photo per question.

A response to a single question might look like:

```json
[
  { "type": "text",  "content": "We finally hiked to the summit! It was freezing..." },
  { "type": "photo", "file_path": "/uploads/2026/03/summit.jpg", "caption": "The view from the top" },
  { "type": "text",  "content": "On the way down we found this tiny café..." },
  { "type": "photo", "file_path": "/uploads/2026/03/cafe.jpg", "caption": "Best hot chocolate ever" },
  { "type": "link",  "link_url": "https://open.spotify.com/track/xxx", "content": "Song that was playing" }
]
```

### SQLite Schema (Key Tables)

```sql
-- Users are never hard-deleted. Deactivation is via is_active = false.
-- All foreign keys referencing users(id) rely on this policy — no ON DELETE CASCADE needed.
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
    token_hash TEXT NOT NULL UNIQUE,       -- SHA-256 hash; raw token only exists in email/URL
    type TEXT NOT NULL CHECK(type IN ('login', 'email_cta')),
    expires_at DATETIME NOT NULL,          -- 30 min for login, 30 days for email CTAs
    consumed_at DATETIME,                  -- set on first use (single-use tokens)
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Server-side sessions: supports revocation and survives restarts
CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    token_hash TEXT NOT NULL UNIQUE,       -- SHA-256 hash of session cookie value
    expires_at DATETIME NOT NULL,          -- 30-day expiry
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- App settings (single-row table, populated by onboarding wizard)
CREATE TABLE settings (
    id INTEGER PRIMARY KEY CHECK(id = 1),  -- enforce single row
    loop_name TEXT NOT NULL DEFAULT 'PiecesOfLife',
    tagline TEXT,
    frequency TEXT NOT NULL DEFAULT 'monthly' CHECK(frequency IN ('biweekly', 'monthly', 'quarterly')),
    submission_window_days INTEGER NOT NULL DEFAULT 7,
    start_datetime DATETIME,                    -- UTC: admin sets local time, app converts to UTC at submission
    timezone TEXT NOT NULL DEFAULT 'UTC',        -- IANA timezone for display and schedule input
    invite_note TEXT,                           -- personal message included in invite emails (set during wizard, reusable)
    setup_complete BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE issues (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT,                -- custom name: inside jokes, vibes, themes
    month INTEGER NOT NULL,
    year INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'collecting', 'published')),
    opens_at DATETIME NOT NULL,
    deadline DATETIME NOT NULL,
    published_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    -- No UNIQUE constraint on (month, year) — biweekly frequency needs multiple issues/month.
    -- "One active issue at a time" enforced at the app level.
);

CREATE TABLE questions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER NOT NULL REFERENCES issues(id),
    text TEXT NOT NULL,
    category TEXT,
    source TEXT NOT NULL DEFAULT 'bank' CHECK(source IN ('bank', 'friend')),
    submitted_by INTEGER REFERENCES users(id),  -- who submitted (friend or admin)
    sort_order INTEGER DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Note: issue_id is intentionally omitted — derive via question_id → questions.issue_id.
-- This avoids inconsistency (a response referencing a question from a different issue).
-- Queries needing "all responses for issue X" JOIN through questions. Acceptable for <50 users.
CREATE TABLE responses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    question_id INTEGER NOT NULL REFERENCES questions(id),
    is_draft BOOLEAN NOT NULL DEFAULT 1,
    version INTEGER NOT NULL DEFAULT 1,  -- optimistic concurrency: incremented on each save
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, question_id)
);

-- Block-based response content: ordered text, photo, and link blocks
CREATE TABLE response_blocks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    response_id INTEGER NOT NULL REFERENCES responses(id) ON DELETE CASCADE,
    type TEXT NOT NULL CHECK(type IN ('text', 'photo', 'link')),
    content TEXT,              -- text content for 'text' blocks, display text for 'link' blocks
    file_path TEXT,            -- for 'photo' blocks: path to uploaded file (original, no compression)
    caption TEXT,              -- for 'photo' blocks: optional caption
    link_url TEXT,             -- for 'link' blocks: Spotify/YouTube URL
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

CREATE TABLE reactions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    response_id INTEGER NOT NULL REFERENCES responses(id),
    emoji TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(user_id, response_id, emoji)
);

-- Pre-generated question bank (seeded with 500+ questions)
CREATE TABLE question_bank (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    text TEXT NOT NULL UNIQUE,
    category TEXT NOT NULL,
    used BOOLEAN NOT NULL DEFAULT 0,   -- semi-unique: prefer unused, allow repeats if bank runs low
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE email_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER REFERENCES users(id),
    issue_id INTEGER REFERENCES issues(id),
    type TEXT NOT NULL CHECK(type IN ('invite', 'open', 'reminder', 'published', 'comment_notification', 'reaction_digest')),
    status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'sent', 'failed')),
    sent_at DATETIME,
    error TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Per-user notification preferences (all default to true)
CREATE TABLE notification_preferences (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL UNIQUE REFERENCES users(id),
    issue_open BOOLEAN NOT NULL DEFAULT 1,
    reminders BOOLEAN NOT NULL DEFAULT 1,
    published BOOLEAN NOT NULL DEFAULT 1,
    comment_notify BOOLEAN NOT NULL DEFAULT 1,
    reaction_notify BOOLEAN NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Tracks scheduler events to prevent re-execution after restarts
CREATE TABLE scheduler_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id INTEGER REFERENCES issues(id), -- NULL for global events (token_cleanup, session_cleanup)
    event_type TEXT NOT NULL CHECK(event_type IN ('reminder_1', 'reminder_2', 'auto_close', 'reaction_digest', 'token_cleanup', 'session_cleanup')),
    -- auto_close triggers the collecting → published transition (compile + send newsletter)
    scheduled_at DATETIME NOT NULL,        -- when the event was supposed to fire
    fired_at DATETIME,                     -- when it actually fired (NULL = pending)
    was_late BOOLEAN NOT NULL DEFAULT 0,   -- true if fired during startup catch-up
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(issue_id, event_type, scheduled_at)  -- allows recurring global events (daily cleanup) while preventing duplicate issue-specific events
);
```

---

## 4. Feature Specification

### 4.1 Authentication — Magic Links

**Flow:**
1. User enters email on login page
2. System generates a cryptographically random token, stores SHA-256 hash in `auth_tokens` table with `type = 'login'` and 30-minute expiry
3. Email sent with link: `https://piecesoflife.yourdomain.com/auth/verify?token=xxx`
4. Clicking link validates token hash → marks token as consumed (`consumed_at`) → creates a row in `sessions` table → sets HTTP-only session cookie (30-day expiry)
5. Expired, consumed, or invalid tokens are rejected

**Session management:** Sessions are stored server-side in the `sessions` table. The session cookie contains a random token; the SHA-256 hash is stored in the DB. This supports:
- Revocation (deactivate a user → delete their sessions)
- Survival across server restarts (unlike in-memory sessions)
- "Log out" actually invalidates the session (deletes the row)

**Why magic links:**
- Zero password management for your friends
- Email is already the primary channel
- Simple to implement securely

**Admin bootstrap:** You (Pari) are seeded as admin via env var or first-run setup. Admin can invite new members by email.

#### Authenticated Email Links (Zero-Click Access)

Every actionable email embeds an auth token in its CTA link, so friends go from inbox to the right page in one click — no separate login step.

**How it works:**
- When sending an "Issue Open" or "Reminder" email, generate a token stored in `auth_tokens` with `type = 'email_cta'` and 30-day expiry, scoped to that user
- Multiple tokens can coexist per user (login tokens don't invalidate email CTA tokens)
- Embed it in the CTA: `https://piecesoflife.yourdomain.com/issues/42/respond?auth=xxx`
- When clicked: validate token hash → mark consumed → create session → redirect to the response page
- If the token is expired, gracefully fall back to the login page with a pre-filled email: "Your link expired — we've sent you a fresh one!"

Typical friend flow: **open email → click green button → start writing**. No friction.

**Security notes:**
- Embedded tokens are single-use (consumed on first click) and scoped to a specific user
- They do NOT grant admin privileges, even if intercepted
- All tokens are hashed (SHA-256) in the database; raw tokens only exist in the email
- Expired/consumed tokens are cleaned up by the scheduler daily (`token_cleanup` event): deletes rows from `auth_tokens` where `consumed_at IS NOT NULL` or `expires_at < NOW()`
- Expired sessions are also cleaned up daily (`session_cleanup` event): deletes rows from `sessions` where `expires_at < NOW()`

### 4.2 Admin Onboarding Wizard (First-Run Setup)

When the admin first accesses PiecesOfLife after deployment, a guided wizard walks through the full setup.

**Wizard Steps:**

```
Step 1          Step 2          Step 3          Step 4          Step 5          Step 6
Set Your     →  Name Your    →  Set the      →  Pick Your    →  Invite Your  →  🎉 Launch!
Name            Loop            Rhythm          First Qs        Friends
```

**Step 1 — Set Your Name**
- Text input: "What should we call you?" (bootstraps the admin user's `name` field)
- Required — `users.name` is NOT NULL and needs a value at creation

**Step 2 — Name Your Loop**
- Text input: "What should we call this?" (e.g., "The Inner Circle", "Chaos Crew")
- Optional: tagline / description
- Defaults pre-filled: name = "PiecesOfLife", editable

**Step 3 — Set the Rhythm**
- Frequency selector: Monthly (recommended, highlighted), Biweekly, Quarterly
- Start date and time: date picker + time picker (admin enters in their local timezone, app converts to UTC for storage)
- Submission window length: 7 days (default, recommended), 10 days, 14 days
- Timezone: auto-detected from browser, editable (default: Europe/Berlin)
- Smart defaults are pre-selected but fully editable — soft guidance, not hard constraints

**Step 4 — Pick Your First Questions**
- System pre-selects 5 questions from the bank (mix of categories)
- Each question shown as a card with category badge
- Actions per card: keep ✓, swap 🔄 (loads another from same category), remove ✗
- "Add your own" button to write a custom question
- Recommendation text: "3–5 questions is the sweet spot" (soft guidance)
- Drag-to-reorder support

**Step 5 — Invite Your Friends**
- Email input field with "Add" button (or paste comma-separated list)
- Shows added friends as avatar chips (letter avatar until they set a photo)
- Recommendation text: "3–10 friends works best" (soft guidance, no hard limit)
- Personal note field: "Write a message that'll be included in their invite email"
  - Placeholder: "Hey! I set up this thing called PiecesOfLife where we answer fun questions about our lives each month. I think you'll love it — join us!"
- Preview of what the invite email will look like (expandable)

**Step 6 — Launch! 🎉**
- Summary of all settings (loop name, frequency, questions, members)
- Big "Launch Your PiecesOfLife" button
- On click: confirmation message + redirect to admin dashboard
- System immediately: creates the first issue, sends invite emails, sends "Issue Open" email
- Redirect to the admin dashboard showing the new issue in COLLECTING state

**Post-wizard:** The wizard only runs once. Subsequent issues are created from the admin dashboard.

### 4.3 Issue Lifecycle

```
  DRAFT → COLLECTING → PUBLISHED
   │          │            │
   │          │            └── Final newsletter sent, web archive visible
   │          └── Submission window open, reminders firing
   └── Questions selected, dates set
```

**Constraint: one active issue at a time.** Only one issue may be in DRAFT or COLLECTING state. A new issue cannot be created until the current one is PUBLISHED. This keeps the experience unambiguous for friends — there's always one clear "current issue." Enforced at the application level (not DB constraint).

**Monthly timeline (configurable):**

| Day | Event | Email Sent |
|-----|-------|------------|
| 1st | Issue opens — questions go live | "New questions are ready!" with authenticated link |
| 7th | First reminder | "Don't forget to share — 1 week left" + group progress |
| 12th | Second reminder (only to non-responders) | "3 days left — we'd love to hear from you" |
| 14th | Deadline — submissions close | — |
| 15th | Issue compiled and published | "Your PiecesOfLife issue is here! 🎉" |

**Progress tracking:** A user counts as "responded" once they have at least one submitted (non-draft) response for the issue. Drafts don't count. Skipped questions are fine — submitting any answers marks the user as responded. Reminders target users with zero submitted responses.

**Admin controls (all accessible as primary action buttons — never hidden in menus):**
- Extend deadline
- Publish Now (compile and send immediately, regardless of deadline)

**Scheduler (goroutine):** The background scheduler runs inside the same Go process. Events are tracked in the `scheduler_events` table to prevent re-execution after restarts.

**Startup behavior (missed-event recovery):**
1. On startup, query `scheduler_events` for rows where `fired_at IS NULL` and `scheduled_at < NOW()`
2. Execute each overdue event (send late reminders, auto-publish, etc.)
3. Mark them as `fired_at = NOW(), was_late = true`
4. Log each late event: `"[SCHEDULER] Firing late event: reminder_1 for issue #7 (was scheduled 2h ago)"`
5. Then schedule remaining future events normally using `time.AfterFunc` or `gocron`

**Normal operation:** When a new issue is created, insert rows into `scheduler_events` for all planned events (reminder_1, reminder_2, auto_close) with `fired_at = NULL`. The `auto_close` event triggers the `collecting` → `published` transition (compile and send newsletter). When fired, update `fired_at = NOW()`. Recalculates on admin actions (extend deadline, etc.).

### 4.4 Question System

**Two sources of questions:**

1. **Question Bank (pre-generated):** A seeded database of 500+ thoughtful questions across categories:
   - Life updates ("What's something that made you smile this week?")
   - Deep thoughts ("What's a belief you've changed your mind about recently?")
   - Fun/silly ("If you could instantly master one skill, what would it be?")
   - Memories ("What's a story from your childhood you think about often?")
   - Goals ("What's one thing you're working toward right now?")
   - Recommendations ("What's something you've consumed recently that everyone should try?")
   - Hypotheticals ("If you had to move to a new country tomorrow, where would you go?")

   Questions are marked `used = true` when included in an issue to prefer freshness.
   If the bank runs low on unused questions, it's fine to repeat — reset `used` annually
   or just allow repeats (people's answers change over time anyway).

2. **Friend-submitted (auto-approved):** Friends can submit questions while an issue is in
   `collecting` state. Submitted questions are attached to the current active issue and
   **automatically included** — no admin approval step. This keeps the loop collaborative
   and removes the admin bottleneck.

   - Friends can submit questions from the response page (prompt at the bottom)
   - Question submission is only available when an issue is in `collecting` state
   - Friend-submitted questions are tagged with `source = 'friend'` and `submitted_by`
   - Admin can still remove questions from an issue, but the default is inclusion

**Question selection flow for a new issue:**
1. System auto-selects 3–5 questions: prefers unused bank questions, includes any pending
   friend-submitted questions
2. Admin can optionally review and swap/reorder, but this step is not required — the
   system can auto-open an issue without admin intervention if configured
3. Issue moves to COLLECTING

### 4.5 Response Submission — Block-Based Editor

This is the core experience for friends. The editor should feel simple and inviting — "writing a message to friends" not "filling out a form."

#### Implementation: Vanilla JS + Go Templates

The response page is server-rendered by Go's `html/template` with oat.ink styling. A small
vanilla JS module (`editor.js`, ~200–300 lines) provides the interactive editor behavior:

- **Auto-save**: `setInterval` → serialize blocks to JSON → `fetch PUT /api/responses/:id/autosave`
- **Block insertion**: click "Add photo" → `createElement` to insert a new block div, trigger file picker
- **Drag-to-reorder**: SortableJS (~10KB, vendored) for touch + desktop drag support
- **Photo upload**: `fetch POST` with `FormData` → server returns file path → insert photo block
- **Link detection**: paste event listener, regex for Spotify/YouTube URLs → auto-create link block

No React, no Svelte, no build step. The JS file is embedded in the Go binary via `go:embed`.

#### Page Layout

```
┌─────────────────────────────────────────────────────────┐
│  PiecesOfLife  ·  March 2026: "Chaos Crew #7"            │
│                                                          │
│  Progress: ████████░░ 3 of 5 answered                   │
│                                     [Save Draft] [Submit]│
├─────────────────────────────────────────────────────────┤
│                                                          │
│  Q1: What's something you're proud of this month?       │
│  ┌───────────────────────────────────────────────────┐  │
│  │ [Start writing...]                                │  │
│  │                                                   │  │
│  ├───────────────────────────────────────────────────┤  │
│  │  📷 Add photo   🔗 Add link   ✕ Clear            │  │
│  └───────────────────────────────────────────────────┘  │
│       ↳ auto-saved 2 min ago                            │
│                                                          │
│  Q2: Share a photo of your week                          │
│  ┌───────────────────────────────────────────────────┐  │
│  │ "Here's what my weekend looked like..."           │  │
│  │                                                   │  │
│  │ ┌──────────┐ ┌──────────┐                        │  │
│  │ │  🖼️ img1 │ │  🖼️ img2 │  [+ Add more]         │  │
│  │ │ "sunset" │ │ "dinner" │                        │  │
│  │ └──────────┘ └──────────┘                        │  │
│  │                                                   │  │
│  │ "The dinner was at that place Alex recommended..." │  │
│  ├───────────────────────────────────────────────────┤  │
│  │  📷 Add photo   🔗 Add link   ✕ Clear            │  │
│  └───────────────────────────────────────────────────┘  │
│       ↳ auto-saved just now                             │
│                                                          │
│  Q3: What question would you ask the group?     [Skip]  │
│  ┌───────────────────────────────────────────────────┐  │
│  │ [Start writing...]                                │  │
│  └───────────────────────────────────────────────────┘  │
│                                                          │
│  ┌───────────────────────────────────────────────────┐  │
│  │ 💡 Got a question for next month?                 │  │
│  │ [Submit a question →]                             │  │
│  └───────────────────────────────────────────────────┘  │
│                                                          │
└─────────────────────────────────────────────────────────┘
```

#### Editor Behavior

**Block composition:**
- Each question starts with an empty text block
- Toolbar below each response area: `📷 Add photo` `🔗 Add link` buttons with tooltips
- Clicking "Add photo" inserts a photo block after the current cursor position (or at the end)
- Photos uploaded via click (file picker) or drag-and-drop onto the response area
- **Upload limits: 50MB max per file, 10 files max per response.** Enforced client-side (file picker rejects oversized files with a friendly message before upload) and server-side (returns 413 with error). Client-side validation: check `file.size` before `fetch`, show inline error "This file is too large (max 50MB)" without a network round-trip.
- **Photos are stored at original size — no compression, no resizing.** What you upload is what gets served.
- Photo blocks show the image with optional caption field and ✕ delete button
- Text blocks auto-split when a photo is inserted mid-text (creating: text → photo → text)
- Link blocks: paste a Spotify/YouTube URL → auto-detected, shows embed preview
- All blocks are drag-to-reorder within a response

**Tooltips and signifiers:**
- Every icon has a tooltip on hover
- Placeholder text in empty text blocks: warm prompts like "Share your thoughts..." or "Tell us more..."
- Photo upload area shows a dashed border on drag-over (clear drop target signifier)

**Auto-save:**
- Responses auto-save every 30 seconds while editing, or immediately on blur
- Visual indicator: "↳ auto-saved just now" / "↳ auto-saved 2 min ago"
- Responses saved with `is_draft = true` until explicitly submitted
- Draft responses only visible to the author
- On return visit, drafts loaded and ready to continue
- **Optimistic concurrency:** Each autosave request includes the current `version` number. The server increments `version` on success. If the submitted version doesn't match (e.g., a concurrent submit happened), the autosave returns 409 Conflict and the client reloads the latest state. This prevents autosave from overwriting an explicit submit.
- **Unsaved changes warning:** `beforeunload` event listener warns the user if they try to close or navigate away with changes since the last successful autosave. Cleared after each successful save or explicit submit.

**Submission:**
- "Submit" button at top (sticky on scroll)
- On submit: all responses marked `is_draft = false`, toast: "Your answers are in! 🎉"
- Friends can edit until deadline (re-submit overwrites)
- "Skip" on individual questions — answering is optional
- Progress indicator: "3 of 5 answered" with visual bar

**Question submission prompt:**
- At the bottom of the response page (only during `collecting` phase): "Got a question for the group? [Submit a question →]"
- Opens an inline form or dialog to type and submit a question
- Auto-approved: attached to the current active issue immediately

**Visibility:** During COLLECTING, members see only their own responses. After PUBLISHED, everyone sees everything.

### 4.6 The Published Issue — Newsletter & Web View

#### Email Version
- Beautiful HTML email with inline CSS (Go `html/template`, no MJML)
- Header: issue title, month/year, group name
- Each question is a section header
- Under each question: all members' responses (name + avatar + text excerpt)
- Text truncated at ~200 chars with "Read more →" linking to web version
- First photo per response shown inline (additional photos linked)
- CTA at bottom: "View the full issue, comment & react →" linking to web

#### Web Version — Layout and Features

**URL:** `/issues/{year}/{month}` (e.g., `/issues/2026/03`)

```
┌──────────────────────────────────────────────────────────────────┐
│  PiecesOfLife · March 2026: "Chaos Crew #7"                       │
│                                                                  │
│  [◉ By Question] [○ By Person]     [≡ List] [⊞ Grid]   [Filter ▼]│
├──────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ── Q1: What's something you're proud of this month? ──────     │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ 🧑 Alex                                                    │  │
│  │ "I finally finished the bathroom renovation! Took 3 months │  │
│  │ but it was worth it..."                                    │  │
│  │                                                            │  │
│  │ [📸 photo: bathroom.jpg]  [📸 photo: before.jpg]           │  │
│  │                                                            │  │
│  │ ❤️ 3  😂 1  🔥 2               💬 2 comments  [Add comment]│  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ 🧑 Maya                                                    │  │
│  │ "Started running again after my knee injury. Did my first  │  │
│  │ 5K in 8 months!"                                          │  │
│  │                                                            │  │
│  │ ❤️ 5  🔥 3                      💬 4 comments  [Add comment]│  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
├──────────────────────────────────────────────────────────────────┤
│  ◀ Feb 2026        Issue Archive         Apr 2026 ▶             │
└──────────────────────────────────────────────────────────────────┘
```

**View modes:**

| Mode | Description |
|------|-------------|
| **By Question** (default) | Responses grouped under each question. |
| **By Person** | All responses from a single friend shown together. |
| **List view** (default) | Full-width cards, text-heavy. |
| **Grid view** | Compact 2-column grid, photo-forward. |

**Filter & Sort:**
- Filter by member (multi-select dropdown)
- Sort: newest first (default), oldest first, most reactions
- Filter by "has photos"

**Photo lightbox:**
- Click any photo → full-screen overlay
- Navigate between photos (← → keys, swipe on mobile)
- Caption shown below
- Close with ✕, click-outside, or Escape

**Widescreen / reading mode:**
- Toggle to expand content to full viewport width
- Stored as user preference (cookie)

**Issue archive:**
- Bottom of each issue: prev/next navigation
- `/issues` page: all published issues with title, date, response count
- Photo collage thumbnail per issue

### 4.7 Photo Albums

**Route:** `/albums`

Cross-issue gallery aggregating all photos from published issues.

- Default: reverse-chronological grid of all photos
- Filter by: issue/month, person, question category
- Click → same lightbox, with context: "From [Name]'s answer to [Question] — [Month Year]"
- Links back to the full response in the issue view
- Backed by a simple query: `SELECT * FROM response_blocks WHERE type = 'photo' ...` with joins

### 4.8 Comments & Reactions

**Comments:**
- Threaded (one level of nesting: top-level + replies)
- Available on each individual response
- Sidebar panel slides in from right when "💬 comments" is clicked (no page navigation)
- Markdown rendered server-side via `goldmark` (Go library). Raw HTML stripped by default (`WithUnsafe(false)`). Output inserted into templates as trusted `template.HTML`.
- Email notification to response author (respects notification preferences)

**Reactions:**
- Fixed set: ❤️ 😂 🔥 👏 🤔 😢
- One-click toggle
- Count + tooltip showing who reacted
- Micro-animation on click

### 4.9 Email System

**Provider:** Fastmail via SMTP (`smtp.fastmail.com:465`, implicit TLS).
Uses an app-specific password created at Fastmail Settings → Password & Security → API tokens.
Sends from your custom domain (e.g., `piecesoflife@yourdomain.com`).
DKIM/SPF/DMARC already configured through Fastmail's domain setup.

**Go implementation:** Use `net/smtp` or `github.com/wneessen/go-mail` for TLS SMTP sending.

**Email types:**

| Email | Trigger | Recipients | Auth Link | Content |
|-------|---------|------------|-----------|---------|
| **Invite** | Admin adds member | New member | Magic link → profile setup | Welcome + admin's personal note |
| **Issue Open** | Issue → COLLECTING | All (pref: issue_open) | Auth link → response page | Questions + "Start answering →" |
| **Reminder 1** | Scheduler (day 7) | All (pref: reminders) | Auth link → response page | Nudge + progress |
| **Reminder 2** | Scheduler (day 12) | Non-responders (pref: reminders) | Auth link → response page | Stronger nudge + deadline |
| **Published** | Issue compiled | All (pref: published) | Auth link → issue view | Full newsletter + "View & comment →" |
| **Comment** | New comment | Response author (pref: comment_notify) | Auth link → response | "[Name] commented on your answer" |
| **Reaction Digest** | Scheduler (daily) | Users with reactions (pref: reaction_notify) | Auth link → issue | "3 people reacted today" |

**Rate limits:** Fastmail allows sending via SMTP with standard rate limits (suitable for a
small group). For ~10 friends and ~7 email types per cycle, you're looking at ~70 emails/month
— well within any limits.

**Templates:** Go-native `html/template` files with inline CSS. No MJML, no Node.js build
step. Templates are embedded in the binary via `go:embed` and rendered with dynamic content
injection before sending. See §8 for design principles.

### 4.10 Admin Dashboard — Layout & UX

The admin dashboard surfaces the most important information at a glance with primary
actions within one click.

#### Main Dashboard (`/admin`)

```
┌──────────────────────────────────────────────────────────────┐
│  PiecesOfLife Admin · "Chaos Crew"                [Settings ⚙]│
├──────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌─ CURRENT ISSUE ──────────────────────────────────────┐   │
│  │                                                      │   │
│  │  March 2026 · "Chaos Crew #7"                        │   │
│  │  Status: COLLECTING · Deadline: March 14              │   │
│  │                                                      │   │
│  │  Submission Progress:                                │   │
│  │  ✅ Alex       ✅ Maya       ⏳ Jordan                │   │
│  │  ✅ Sam        ⏳ Riley      ⏳ Casey                 │   │
│  │  ⏳ Jamie      ✅ Morgan                              │   │
│  │                                                      │   │
│  │  5 of 8 friends have responded                       │   │
│  │  ████████████░░░░░░ 62%                              │   │
│  │                                                      │   │
│  │  [🚀 Publish Now]  [⏰ Extend Deadline]              │   │
│  │  [📧 Send Reminder to Non-Responders]                │   │
│  │                                                      │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│                                                              │
│  ┌─ QUICK ACTIONS ──────────────────────────────────────┐   │
│  │  [+ New Issue]  [👥 Manage Members]  [❓ Question Bank]│   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ PAST ISSUES ────────────────────────────────────────┐   │
│  │  Feb 2026 · "Chaos Crew #6" · 8/8 responded · View  │   │
│  │  Jan 2026 · "Chaos Crew #5" · 7/8 responded · View  │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ EMAIL LOG ──────────────────────────────────────────┐   │
│  │  Last 5 emails · [View all →]                        │   │
│  │  ✅ Issue Open → Alex (Mar 1) · Delivered             │   │
│  │  ❌ Issue Open → Casey (Mar 1) · Bounced [Resend]    │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

#### Design Principles

1. **Primary actions always visible** — "Publish Now", "Extend Deadline", "Send Reminder" are buttons, not menu items
2. **Submission progress front and center** — who responded, who hasn't, at a glance
3. **Friend questions visible** — auto-included submissions shown with checkmarks; admin can remove if needed but default is inclusion
4. **Settings on a dedicated page** — `/admin/settings`, not crammed into a dropdown
5. **Confirmations on destructive actions** — "Publish Now" shows a confirmation modal

#### Member Management (`/admin/members`)
- Member list: name, email, avatar, status (active/invited/deactivated), join date
- Actions: re-send invite, deactivate, promote to admin
- "Invite New Member" form at top

#### Question Bank (`/admin/questions`)
- All bank questions grouped by category
- Tags: "used" / "unused" / "friend-submitted"
- Actions: edit, retire, add new
- Filter by category, source, used/unused

---

## 5. API Design

**Versioning policy:** No API versioning, no deprecation process. This is a single-user self-hosted app — breaking changes are made when needed and deployed directly. Git history serves as the changelog.

### Routes

The Go monolith serves both **page routes** (HTML via `html/template`) and **API routes** (JSON).
Page routes handle navigation; API routes handle AJAX calls from the editor, comments, reactions, etc.

### Response Format Conventions

All API routes return JSON with `Content-Type: application/json`.

**Single resource:**
```json
{"id": 1, "name": "Alex", "email": "alex@example.com"}
```

**List of resources** (paginated):
```json
{"items": [...], "total": 42, "page": 1, "per_page": 50}
```

All list endpoints accept optional `?page=1&per_page=50` query params. Default `per_page` is 50. Max `per_page` is 100. Offset-based (`LIMIT/OFFSET` in SQLite). Applied to: `/api/albums`, `/api/issues/:id/responses`, `/api/question-bank`, `/api/admin/email-log`. Other list endpoints (users, issues, questions per issue) are small enough to return unpaginated.

**Success with no body** (e.g., DELETE, toggle): HTTP 204 No Content.

**Error responses** (4xx/5xx):
```json
{"error": {"code": "not_found", "message": "Issue not found"}}
```

Common error codes: `not_found`, `unauthorized`, `forbidden`, `validation_error`, `conflict` (409 for version mismatch), `too_large` (413 for upload limits).

**Validation errors** include field-level detail:
```json
{"error": {"code": "validation_error", "message": "Invalid input", "fields": {"email": "required"}}}
```

```
SYSTEM ROUTES:
  GET    /health                          — health check (returns 200 + {"status":"ok","db":"ok","scheduler":"ok"})
                                            Verifies SQLite is reachable and scheduler goroutine is alive. Used by Traefik/Docker.
  GET    /uploads/*                       — authenticated file serving (session required, serves from /data/uploads via http.ServeFile)

PAGE ROUTES (server-rendered HTML):
  GET    /                                — landing / redirect to current issue or login
  GET    /login                           — magic link request form
  GET    /auth/verify                     — verify magic link token, set session, redirect
  GET    /issues                          — issue archive (all published)
  GET    /issues/{year}/{month}           — published issue view (with view/filter query params)
  GET    /issues/{id}/respond             — response editor (collecting phase)
  GET    /albums                          — photo album gallery
  GET    /profile                         — user profile + notification preferences
  GET    /admin                           — admin dashboard
  GET    /admin/members                   — member management
  GET    /admin/questions                 — question bank
  GET    /admin/settings                  — app settings
  GET    /admin/setup                     — onboarding wizard (first-run only)

API ROUTES (JSON, called by JS):
  Authentication:
    POST   /api/auth/login                — request magic link
    POST   /api/auth/logout               — clear session
    GET    /api/auth/me                   — current user info

  Users:
    GET    /api/users                     — list all members
    POST   /api/users/invite              — invite new member (admin). Error if email is active member. Reactivates + resends invite if deactivated.
    PATCH  /api/users/:id                 — update profile (name, avatar, bio) or deactivate (admin: {"is_active": false})
    GET    /api/users/:id/preferences     — notification preferences
    PATCH  /api/users/:id/preferences     — update preferences

  Onboarding:
    POST   /api/onboarding/complete       — submit wizard data (admin, first-run)

  Issues:
    GET    /api/issues                    — list issues (?status= filter)
    POST   /api/issues                    — create issue (admin)
    GET    /api/issues/:id                — issue details + questions
    PATCH  /api/issues/:id                — update title/dates (admin)
    POST   /api/issues/:id/publish        — compile and publish (admin)
    POST   /api/issues/:id/extend         — extend deadline (admin)
    GET    /api/issues/:id/progress       — submission progress


  Questions:
    GET    /api/issues/:id/questions      — questions for an issue
    POST   /api/issues/:id/questions      — add question (admin)
    PATCH  /api/questions/:id             — edit question (admin)
    DELETE /api/questions/:id             — remove question (admin)
    POST   /api/questions/submit          — friend submits a question (auto-approved, requires active collecting issue)

  Responses:
    GET    /api/issues/:id/responses      — all responses (published only)
                                            ?view=by_question|by_person
                                            &member=:userId &sort=newest|oldest|reactions
                                            &has_photos=true
    GET    /api/issues/:id/responses/mine — current user's responses + drafts
    POST   /api/responses                 — create response (returns ID)
    DELETE /api/responses/:id             — delete own response
    POST   /api/responses/:id/submit      — mark as submitted (is_draft → false)

  Response Blocks:
    GET    /api/responses/:id/blocks      — ordered blocks for a response
    POST   /api/responses/:id/blocks      — add a block
    PATCH  /api/blocks/:id                — update block content/caption
    DELETE /api/blocks/:id                — delete a block
    POST   /api/responses/:id/blocks/reorder — reorder (accepts ordered ID array)
    POST   /api/responses/:id/blocks/upload  — upload photo → create photo block

  Auto-Save:
    PUT    /api/responses/:id/autosave    — bulk upsert all blocks (called every 30s)
                                            Requires `version` in body for optimistic concurrency.
                                            Returns 409 Conflict if version mismatch.

  Comments:
    GET    /api/responses/:id/comments    — comments on a response
    POST   /api/responses/:id/comments    — add comment
    DELETE /api/comments/:id              — delete (author or admin)

  Reactions:
    POST   /api/responses/:id/reactions   — toggle reaction { emoji: "❤️" }
    GET    /api/responses/:id/reactions   — reactions with user names

  Albums:
    GET    /api/albums                    — all photos (?issue= &member= &category=)

  Admin:
    GET    /api/admin/email-log           — email delivery log
    POST   /api/admin/resend/:logId       — resend failed email
    POST   /api/admin/send-reminder/:issueId — manual reminder
    GET    /api/admin/settings            — app settings
    PATCH  /api/admin/settings            — update settings

  Question Bank:
    GET    /api/question-bank             — browse (?category= &used= &source=)
    POST   /api/question-bank             — add to bank (admin)
    PATCH  /api/question-bank/:id         — edit (admin)
    DELETE /api/question-bank/:id         — retire (admin)
```

---

## 6. Deployment

### Docker Compose

```yaml
version: "3.8"

services:
  piecesoflife:
    image: piecesoflife:latest
    container_name: piecesoflife
    restart: unless-stopped
    logging:
      driver: json-file
      options:
        max-size: "100m"
        max-file: "3"
    environment:
      - DATABASE_PATH=/data/piecesoflife.db
      - UPLOAD_PATH=/data/uploads
      - SMTP_HOST=smtp.fastmail.com
      - SMTP_PORT=465
      - SMTP_USER=${FASTMAIL_USER}           # your Fastmail email address
      - SMTP_PASS=${FASTMAIL_APP_PASSWORD}   # app-specific password from Fastmail
      - SMTP_TLS=implicit                    # Fastmail uses implicit TLS on 465
      - FROM_EMAIL=piecesoflife@yourdomain.com
      - BASE_URL=https://piecesoflife.yourdomain.com
      - SESSION_SECRET=${SESSION_SECRET}
      - ADMIN_EMAIL=${ADMIN_EMAIL}           # your email, seeded as admin on first run
      - LOG_LEVEL=info
      - LOG_FORMAT=json
      # No TZ env var — container runs in UTC. Display timezone configured via settings.timezone.
    healthcheck:
      test: ["CMD", "wget", "--spider", "-q", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
    volumes:
      - piecesoflife-data:/data
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.piecesoflife.rule=Host(`piecesoflife.yourdomain.com`)"
      - "traefik.http.routers.piecesoflife.entrypoints=websecure"
      - "traefik.http.routers.piecesoflife.tls.certresolver=letsencrypt"
      - "traefik.http.services.piecesoflife.loadbalancer.server.port=8080"
    networks:
      - traefik

volumes:
  piecesoflife-data:

networks:
  traefik:
    external: true
```

### Backup Strategy

Backups handled at the host level: VM/host snapshots or offline backup by stopping the service and copying the `/data` volume (SQLite DB + uploads). No continuous replication needed.

### Build

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o piecesoflife .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata wget
COPY --from=build /app/piecesoflife /usr/local/bin/
EXPOSE 8080
CMD ["piecesoflife"]
```

The binary embeds all templates, static assets (oat.ink CSS/JS, editor.js), and email
templates via `go:embed`. Zero runtime file dependencies. No MJML, no Node.js — email
templates are plain Go `html/template` files with inline CSS.

---

## 7. Question Bank

### Seeding

The question bank is pre-populated with 500+ questions across 7 categories. These are
embedded in the Go binary as a JSON or SQL seed file and loaded on first run.

**Generation strategy:** Use Claude (offline, one-time) to generate the full bank:

```
Generate 75 unique, thoughtful questions for each of these 7 categories,
designed for a close friend group monthly newsletter:

Categories: life_updates, deep_thoughts, fun_silly, memories, goals,
recommendations, hypotheticals

Requirements:
- Warm and inviting tone (not interrogative)
- Appropriate for adults who know each other well
- Mix of light and deep within each category
- No duplicates across categories

Output as JSON: [{"text": "...", "category": "..."}]
```

Run this once, review the output, save as `questions_seed.json`, embed in the binary.

**Replenishment:** If the bank runs low after a couple years, regenerate a new batch
offline and ship it as a DB migration. No runtime API calls needed.

### Selection Algorithm

When creating a new issue, the system selects questions:

1. Fill slots (up to 5 total) from the bank, preferring `used = false`
2. If all bank questions are used, select randomly (repeats are fine — answers change over time)
3. Mark selected bank questions as `used = true`
4. Friends may also submit additional questions during `collecting` phase (auto-approved)

---

## 8. Email Templates

### Design Principles
- Mobile-first responsive HTML
- System fonts (no web font loading in email)
- Inline CSS (email clients strip `<style>` tags)
- Dark mode support via `@media (prefers-color-scheme: dark)`
- 600px max width, centered
- Every CTA button includes an authenticated magic link
- Warm, personal tone — emails to friends, not marketing blasts

### Template List
1. **Invite** — Welcome + admin's personal note + magic link to set up profile
2. **Issue Open** — This month's questions listed + "Start Answering →" auth button
3. **Reminder** — Friendly nudge + progress bar + auth link
4. **Published Issue** — Full newsletter with response excerpts + "View & Comment →"
5. **Comment Notification** — "[Name] commented on your answer" + excerpt + auth link
6. **Reaction Digest** — Daily: "3 people loved your answers today" + auth link

### Build Pipeline
Templates are written directly as Go `html/template` files with inline CSS. No MJML, no
Node.js, no compile step. Templates are embedded in the binary via `go:embed` and rendered
with Go's `html/template` for dynamic content injection before sending.

**Approach:** Use a shared base email template defining the 600px centered layout, system
fonts, inline styles, and dark mode `@media` query. Each email type extends the base with
its specific content blocks. This is more verbose than MJML but keeps the build pipeline
pure Go with zero external dependencies.

### Error Handling

- **SMTP failure mid-batch** (e.g., 3 of 8 emails sent): log each failure in `email_log` with `status = 'failed'` and `error` text, continue sending to remaining recipients. Admin can resend failed ones from the dashboard.
- **SQLite busy/locked**: handled by `busy_timeout=5000` PRAGMA — waits up to 5 seconds before returning an error. Return 503 to the client if the timeout is exceeded.
- **Photo upload failure** (mid-stream, disk full, etc.): return error, don't create the block. Client shows "Upload failed, try again" inline message.
- **General principle**: log the error, return a meaningful HTTP status + error JSON (see Response Format Conventions), never crash the process. The scheduler and HTTP server are independent — a handler panic is recovered by middleware and logged, it doesn't kill the scheduler.

---

## 9. Security & Privacy

| Concern | Approach |
|---------|----------|
| **Authentication** | Magic links (30 min for login, 30 days for email CTAs). Server-side sessions in `sessions` table. HTTP-only secure session cookies (30-day expiry, `SameSite=Lax`). |
| **Authorization** | Role-based: admin vs member. Members edit own responses only. |
| **Email auth tokens** | Single-use, user-scoped, SHA-256 hashed in `auth_tokens` table. Multiple tokens can coexist per user. Cannot escalate to admin. |
| **Data privacy** | Self-hosted on your hardware. No analytics, no third parties. |
| **Photo uploads** | Validate type (JPEG/PNG/WebP). **No EXIF stripping, no compression — originals stored as-is.** Private group, no public exposure. Max 50MB per file, 10 files per response. Enforced client-side and server-side. |
| **Photo serving** | Authenticated via session cookie check (`/uploads/*` route). No static file serving — prevents direct URL access without a valid session. Served via `http.ServeFile`. |
| **CSRF** | Double-submit cookie pattern: server sets a random `csrf_token` cookie (not HTTP-only, so JS can read it); JS sends it as `X-CSRF-Token` header on every `fetch` request; server validates header matches cookie. Stateless, no server-side token storage needed. Applied to all state-changing requests (POST/PATCH/PUT/DELETE). |
| **Security headers** | Set via middleware on all responses: `Content-Security-Policy` (restrict inline scripts, prevent uploaded SVG/HTML execution), `X-Content-Type-Options: nosniff` (prevent MIME-sniffing uploads as HTML/JS), `X-Frame-Options: DENY` (prevent clickjacking via iframes). |
| **Rate limiting** | Magic link requests: 3 per email per hour. |
| **Backups** | Host-level snapshots or offline backup (stop service, copy `/data` volume). |
| **Email** | Fastmail handles DKIM/SPF/DMARC for your domain. |
| **Timestamps** | All timestamps stored as UTC in the database. Converted to configured timezone for display only. |

---

## 10. Remaining Design Questions

All architecture questions are now resolved:

- **SQLite driver:** `modernc.org/sqlite` (pure Go, no CGO). Matches `CGO_ENABLED=0` in Dockerfile. Simpler builds, no C compiler needed.
- **HTTP router:** stdlib `net/http` (Go 1.22+ with pattern matching). Sufficient for this app's complexity.
- **Issue creation:** Admin-triggered (manual). Auto-create on schedule deferred to Phase 4.

---

## 11. Implementation Phases

### Phase 1a — Foundation (Auth + Wizard + Text Responses + Email)
- [ ] Go project scaffold with `go:embed` for static assets
- [ ] SQLite schema + migration runner (including `settings`, `auth_tokens`, `sessions`, `scheduler_events` tables)
- [ ] Embed oat.ink CSS/JS as static assets
- [ ] Magic link auth (login page + email verification + server-side sessions)
- [ ] Authenticated email links (embed tokens in all CTAs via `auth_tokens` table)
- [ ] Admin onboarding wizard (Steps 1–6, includes "set your name" as step 1, writes to `settings` table)
- [ ] Seed question bank (500+ questions from pre-generated JSON)
- [ ] Issue creation with auto-selected questions
- [ ] Response editor: text blocks only (server-rendered + minimal JS for auto-save)
- [ ] Fastmail SMTP integration (send invite + issue open emails)
- [ ] Manual publish: compile responses into newsletter email
- [ ] Web view of published issues (list view, by-question grouping)
- [ ] Dockerfile + Docker Compose + Traefik labels
- [ ] `/health` endpoint for Docker/Traefik health checks
- [ ] Graceful shutdown (SIGTERM handling, in-flight request draining)
- [ ] Structured logging (see §14)

### Phase 1b — Photos + Scheduler + Polish
- [ ] Block-based editor: photo upload, drag-to-reorder (upgrade text-only editor from 1a)
- [ ] Photo upload with limits (50MB/file, 10 files/response), original storage (no EXIF stripping)
- [ ] Auto-save drafts (30-second interval via fetch, optimistic concurrency with `version`)
- [ ] Background scheduler goroutine (reminders, auto-publish, missed-event recovery on startup)
- [ ] Scheduler cleanup events (token_cleanup, session_cleanup — daily)
- [ ] Reminder emails (automated via scheduler)

### Phase 2 — Social Features & Admin Polish
- [ ] Link block detection (Spotify/YouTube URL paste → auto-create link block)
- [ ] Friend question submission (auto-approved, prompt on response page)
- [ ] Comment threads (sidebar panel, vanilla JS)
- [ ] Emoji reactions
- [ ] Admin dashboard (progress tracker, action buttons)
- [ ] Email delivery logging + resend

### Phase 3 — Polish & Views
- [ ] Published issue: grid view toggle, by-person view, filter/sort
- [ ] Photo lightbox with keyboard navigation
- [ ] Photo albums page (cross-issue gallery)
- [ ] Beautiful newsletter email templates (polished inline-CSS HTML)
- [ ] Notification preferences per user
- [ ] Profile page (avatar upload, bio, preferences)
- [ ] Widescreen reading mode
- [ ] Issue archive with photo collage thumbnails

### Phase 4 — Nice to Have
- [ ] PWA manifest + offline support
- [ ] "Mementos" — shareable social cards from responses
- [ ] Link blocks: Spotify/YouTube embed previews
- [ ] Export: download all your data as JSON/ZIP
- [ ] Multiple loops (different friend groups)
- [ ] Dark mode on web UI
- [ ] Auto-create issues on schedule (fully autonomous mode)
- [ ] Customizable theme/accent color

---

## 12. Estimated Resource Requirements

| Resource | Estimate |
|----------|----------|
| **Memory** | ~50–80MB (Go single binary) |
| **Disk** | ~100MB base + ~100MB/month (original photos, no compression, max 50MB/file, 10 files/response) |
| **CPU** | Negligible — spikes only during email sends |
| **Network** | Minimal — <50 users, low traffic |
| **External deps** | Fastmail SMTP only. No APIs, no cloud services. |

Fits comfortably alongside your other homelab services.

---

## 13. Graceful Shutdown

The Go binary handles `SIGTERM` and `SIGINT` for clean shutdown:

1. **Stop accepting new connections** — call `http.Server.Shutdown(ctx)` with a 30-second timeout
2. **Drain in-flight requests** — `Shutdown` waits for active handlers to complete
3. **Stop the scheduler** — cancel the scheduler context; in-progress events finish, pending timers are cancelled (they're persisted in `scheduler_events` with `fired_at = NULL` and will be picked up on next startup)
4. **Close the database** — ensure SQLite WAL is flushed and the DB handle is closed cleanly
5. **Log shutdown** — `"[SHUTDOWN] Graceful shutdown complete"` or `"[SHUTDOWN] Forced shutdown after timeout"`

```go
// Pseudocode
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
<-quit
log.Info("Shutting down...")
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
server.Shutdown(ctx)
scheduler.Stop()
db.Close()
```

---

## 14. Logging Guide

All logging uses Go's `log/slog` (structured logging, stdlib since Go 1.21). JSON format in production, text format in development.

### Log Levels

| Level | Usage |
|-------|-------|
| **INFO** | Normal operations: server start, issue created, email sent, scheduler events fired |
| **WARN** | Recoverable issues: email delivery failed (will retry), token expired, upload rejected |
| **ERROR** | Unexpected failures: DB errors, SMTP connection failures, unhandled panics |
| **DEBUG** | Development only: request details, SQL queries, template rendering |

### What to Log

| Event | Level | Fields |
|-------|-------|--------|
| Server start | INFO | `port`, `version`, `db_path` |
| Request completed | INFO | `method`, `path`, `status`, `duration_ms`, `user_id` |
| Auth token created | INFO | `user_id`, `type` (login/email_cta) |
| Auth token consumed | INFO | `user_id`, `type` |
| Auth token rejected | WARN | `reason` (expired/consumed/invalid) |
| Email sent | INFO | `type`, `user_id`, `issue_id` |
| Email failed | WARN | `type`, `user_id`, `error` |
| Scheduler event fired | INFO | `event_type`, `issue_id`, `was_late` |
| Photo upload | INFO | `user_id`, `file_size`, `response_id` |
| Photo rejected | WARN | `user_id`, `reason` (too large/wrong type/limit exceeded) |
| Autosave conflict | WARN | `response_id`, `expected_version`, `actual_version` |
| Graceful shutdown | INFO | `reason` (SIGTERM/SIGINT) |
| DB error | ERROR | `query`, `error` |

### What NOT to Log
- Token values (raw or hashed)
- Email addresses in plaintext (use user_id)
- Request/response bodies
- Session cookie values

### Configuration
- `LOG_LEVEL` env var: `debug`, `info`, `warn`, `error` (default: `info`)
- `LOG_FORMAT` env var: `json` (default, for production), `text` (for development)

---

## 15. Go Project Structure

```
piecesoflife/
├── main.go                   # entrypoint: HTTP server + scheduler goroutine
├── go.mod
├── go.sum
├── Dockerfile
├── docker-compose.yml
│
├── internal/
│   ├── server/               # HTTP handlers (page routes + API routes)
│   │   ├── routes.go
│   │   ├── auth.go
│   │   ├── issues.go
│   │   ├── responses.go
│   │   ├── comments.go
│   │   ├── admin.go
│   │   └── middleware.go     # auth, CSRF, rate limiting
│   │
│   ├── db/                   # SQLite queries + migrations
│   │   ├── db.go
│   │   ├── migrations/
│   │   └── queries.go        # or use sqlc for type-safe queries
│   │
│   ├── email/                # SMTP sending + template rendering
│   │   ├── sender.go
│   │   └── templates.go
│   │
│   ├── scheduler/            # background job runner
│   │   └── scheduler.go
│   │
│   └── models/               # shared types
│       └── models.go
│
├── static/                   # go:embed'd static assets
│   ├── oat.css               # vendored oat.ink
│   ├── oat.js
│   ├── sortable.min.js       # vendored SortableJS (~10KB) for touch + desktop drag
│   ├── editor.js             # block editor vanilla JS
│   └── app.css               # custom styles
│
├── templates/                # go:embed'd html/template files
│   ├── layouts/
│   │   └── base.html         # shared layout with oat.ink, nav, etc.
│   ├── pages/
│   │   ├── login.html
│   │   ├── respond.html      # response editor
│   │   ├── issue.html        # published issue view
│   │   ├── albums.html
│   │   ├── profile.html
│   │   └── admin/
│   │       ├── dashboard.html
│   │       ├── members.html
│   │       ├── questions.html
│   │       ├── settings.html
│   │       └── setup.html    # onboarding wizard
│   └── email/                # Go html/template email templates (inline CSS, no MJML)
│       ├── invite.html
│       ├── issue_open.html
│       ├── reminder.html
│       ├── published.html
│       ├── comment.html
│       └── reaction_digest.html
│
└── seed/
    └── questions.json        # 500+ pre-generated questions
```

---

## Appendix A: UX Decisions Informed by a similar service Critique

| a similar service Issue | PiecesOfLife Solution |
|---|---|
| Only one photo per question | Block-based responses: unlimited photos interleaved with text |
| No tooltips on icons | All icons have hover tooltips |
| "Send Now" buried in three-dot menu | "Publish Now" is a primary button on admin dashboard |
| Settings in a small chat widget | Dedicated full-page settings at `/admin/settings` |
| No list/grid view options | Toggle between list and grid on published issues |
| No widescreen mode | Widescreen reading toggle, stored as preference |
| Single view mode | Toggle: "By Question" or "By Person" grouping |
| No filter/sort on published issues | Filter by member, photos; sort by newest, oldest, reactions |
| Admin must approve friend questions | Friend questions auto-approved — collaborative by default |
| Confetti on setup completion | Removed — clean confirmation message instead |

## Appendix B: Resolved Architecture Decisions

| Decision | Resolution | Rationale |
|----------|------------|-----------|
| Backend language | **Go** | Single binary, low memory, goroutines for scheduler |
| Frontend framework | **None — Go `html/template` + oat.ink** | No build step, no Node.js, semantic HTML |
| Email templates | **Go `html/template` with inline CSS** | No MJML, no Node.js build dependency, pure Go |
| Auth tokens | **Separate `auth_tokens` table** | Supports concurrent login + email CTA tokens per user |
| Sessions | **Server-side `sessions` table** | Supports revocation, survives restarts |
| Timestamps | **UTC everywhere** | Stored as UTC, converted to configured timezone for display |
| Monolith vs separate | **Monolith** | Single binary serves pages + API + scheduler |
| Scheduler | **Merged — goroutine** | Same process, no IPC needed |
| File storage | **Filesystem (Docker volume)** | Simple, fast, easy backup |
| Email provider | **Fastmail SMTP** | Existing custom domain, app-specific password |
| Question generation | **Pre-generated bank (500+)** | Offline generation, no runtime API dependency |
| Photo handling | **Original size, no compression, no EXIF stripping** | Max 50MB/file, 10 files/response, client+server enforced. Private group. |
| Database | **SQLite** | Zero-ops, single file, host-level backup |
