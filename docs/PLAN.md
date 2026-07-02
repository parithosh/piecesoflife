# PiecesOfLife — Implementation Plan

This document specifies every implementation detail needed to build PiecesOfLife from the spec. It is organized to eliminate ambiguity during coding.

---

## 0. Project Initialization

### 0.1 Go Module

```bash
go mod init github.com/parithosh/piecesoflife
```

### 0.2 Dependencies

```
modernc.org/sqlite          # pure-Go SQLite driver (database/sql compatible)
github.com/wneessen/go-mail # SMTP client with implicit TLS support
github.com/yuin/goldmark    # Markdown→HTML for comments
go-co-op/gocron/v2          # scheduler (preferred over raw time.AfterFunc for cron-like scheduling)
```

No other external dependencies. Everything else is stdlib: `net/http`, `html/template`, `crypto/rand`, `crypto/sha256`, `database/sql`, `encoding/json`, `log/slog`, `embed`, `os/signal`, `context`, `time`, `path/filepath`, `io`, `mime/multipart`, `strings`, `strconv`, `fmt`, `net/url`, `sync`.

### 0.3 Directory Structure (create all at once)

```
piecesoflife/
├── main.go
├── go.mod
├── go.sum
├── Dockerfile
├── docker-compose.yml
│
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── server/
│   │   ├── server.go          # Server struct, route registration, Start()
│   │   ├── routes.go          # mux setup — all route registrations in one place
│   │   ├── middleware.go       # all middleware: auth, csrf, security headers, logging, recovery, admin
│   │   ├── helpers.go          # JSON response helpers, error helpers, pagination parsing
│   │   ├── auth.go             # handlers: login page, request magic link, verify, logout
│   │   ├── onboarding.go       # handlers: wizard page + POST /api/onboarding/complete
│   │   ├── issues.go           # handlers: issue CRUD, publish, extend
│   │   ├── questions.go        # handlers: question CRUD, friend submit
│   │   ├── responses.go        # handlers: response CRUD, autosave, submit, blocks, upload
│   │   ├── comments.go         # handlers: comment CRUD
│   │   ├── reactions.go        # handlers: reaction toggle + list
│   │   ├── albums.go           # handlers: photo album
│   │   ├── admin.go            # handlers: dashboard, members, settings, email log, resend
│   │   ├── pages.go            # handlers: landing, issue view, profile, issue archive
│   │   └── uploads.go          # handler: authenticated file serving
│   │
│   ├── db/
│   │   ├── db.go               # OpenDB(), PRAGMA setup, migration runner
│   │   ├── migrations/
│   │   │   └── 001_initial.sql
│   │   ├── users.go            # user queries
│   │   ├── auth.go             # auth_tokens + sessions queries
│   │   ├── settings.go         # settings queries
│   │   ├── issues.go           # issue queries
│   │   ├── questions.go        # questions + question_bank queries
│   │   ├── responses.go        # responses + response_blocks queries
│   │   ├── comments.go         # comment queries
│   │   ├── reactions.go        # reaction queries
│   │   ├── email_log.go        # email_log queries
│   │   ├── notifications.go    # notification_preferences queries
│   │   └── scheduler.go        # scheduler_events queries
│   │
│   ├── email/
│   │   ├── sender.go           # SMTP client, SendEmail()
│   │   └── templates.go        # email template loading + rendering
│   │
│   ├── scheduler/
│   │   └── scheduler.go        # Scheduler struct, Start(), Stop(), event handlers
│   │
│   └── models/
│       └── models.go           # all shared types
│
├── static/                     # go:embed'd
│   ├── oat.css
│   ├── oat.js
│   ├── sortable.min.js
│   ├── editor.js
│   └── app.css
│
├── templates/                  # go:embed'd
│   ├── layouts/
│   │   └── base.html
│   ├── pages/
│   │   ├── login.html
│   │   ├── respond.html
│   │   ├── issue.html
│   │   ├── issues_archive.html
│   │   ├── albums.html
│   │   ├── profile.html
│   │   └── admin/
│   │       ├── dashboard.html
│   │       ├── members.html
│   │       ├── questions.html
│   │       ├── settings.html
│   │       └── setup.html
│   └── email/
│       ├── base.html
│       ├── invite.html
│       ├── issue_open.html
│       ├── reminder.html
│       ├── published.html
│       ├── comment.html
│       └── reaction_digest.html
│
└── seed/
    └── questions.json
```

---

## 1. Configuration (`internal/config/config.go`)

### 1.1 Config Struct

```go
type Config struct {
    // Server
    Port    int    // default 8080
    BaseURL string // e.g., "https://piecesoflife.yourdomain.com"
    DevMode bool   // DEV_MODE=true → read templates/static from filesystem

    // Database
    DatabasePath string // e.g., "/data/piecesoflife.db"

    // Uploads
    UploadPath string // e.g., "/data/uploads"

    // SMTP
    SMTPHost string // smtp.fastmail.com
    SMTPPort int    // 465
    SMTPUser string
    SMTPPass string
    SMTPTLS  string // "implicit" for port 465
    FromEmail string // piecesoflife@yourdomain.com

    // Auth
    SessionSecret string // used to derive CSRF token signing (not used for session tokens — those are random)
    AdminEmail    string // seeded as admin on first run

    // Logging
    LogLevel  string // debug, info, warn, error
    LogFormat string // json, text
}
```

### 1.2 Loading

```go
func Load() (*Config, error)
```

Read each field from `os.Getenv()`. Apply defaults for optional fields (Port=8080, LogLevel="info", LogFormat="json", SMTPTLS="implicit"). Validate required fields: `DatabasePath`, `UploadPath`, `SMTPHost`, `SMTPPort`, `SMTPUser`, `SMTPPass`, `FromEmail`, `BaseURL`, `SessionSecret`, `AdminEmail`. Return descriptive error for any missing required field.

### 1.3 Gotcha: SESSION_SECRET

`SESSION_SECRET` is used solely for CSRF token derivation (HMAC signing of the random CSRF cookie). Session tokens themselves are cryptographically random and stored as SHA-256 hashes — they don't use this secret. The naming is from the docker-compose spec. Document this in a comment.

---

## 2. Models (`internal/models/models.go`)

Every struct maps to a DB table or an API response shape. JSON tags use `snake_case`. Fields that may be NULL in the DB use pointer types.

```go
type User struct {
    ID        int64      `json:"id"`
    Name      string     `json:"name"`
    Email     string     `json:"email"`
    AvatarURL *string    `json:"avatar_url"`
    Bio       *string    `json:"bio"`
    Role      string     `json:"role"`       // "admin" | "member"
    IsActive  bool       `json:"is_active"`
    CreatedAt time.Time  `json:"created_at"`
}

type AuthToken struct {
    ID         int64      `json:"-"`
    UserID     int64      `json:"-"`
    TokenHash  string     `json:"-"`
    Type       string     `json:"-"` // "login" | "email_cta"
    ExpiresAt  time.Time  `json:"-"`
    ConsumedAt *time.Time `json:"-"`
    CreatedAt  time.Time  `json:"-"`
}

type Session struct {
    ID        int64     `json:"-"`
    UserID    int64     `json:"-"`
    TokenHash string    `json:"-"`
    ExpiresAt time.Time `json:"-"`
    CreatedAt time.Time `json:"-"`
}

type Settings struct {
    ID                   int64      `json:"id"`
    LoopName             string     `json:"loop_name"`
    Tagline              *string    `json:"tagline"`
    Frequency            string     `json:"frequency"`       // "biweekly" | "monthly" | "quarterly"
    SubmissionWindowDays int        `json:"submission_window_days"`
    StartDatetime        *time.Time `json:"start_datetime"`  // UTC
    Timezone             string     `json:"timezone"`         // IANA, e.g., "Europe/Berlin"
    InviteNote           *string    `json:"invite_note"`
    SetupComplete        bool       `json:"setup_complete"`
    CreatedAt            time.Time  `json:"created_at"`
    UpdatedAt            time.Time  `json:"updated_at"`
}

type Issue struct {
    ID          int64      `json:"id"`
    Title       *string    `json:"title"`
    Month       int        `json:"month"`
    Year        int        `json:"year"`
    Status      string     `json:"status"` // "draft" | "collecting" | "published"
    OpensAt     time.Time  `json:"opens_at"`
    Deadline    time.Time  `json:"deadline"`
    PublishedAt *time.Time `json:"published_at"`
    CreatedAt   time.Time  `json:"created_at"`
}

type Question struct {
    ID          int64     `json:"id"`
    IssueID     int64     `json:"issue_id"`
    Text        string    `json:"text"`
    Category    *string   `json:"category"`
    Source      string    `json:"source"` // "bank" | "friend"
    SubmittedBy *int64    `json:"submitted_by"`
    SortOrder   int       `json:"sort_order"`
    CreatedAt   time.Time `json:"created_at"`
}

type Response struct {
    ID         int64     `json:"id"`
    UserID     int64     `json:"user_id"`
    QuestionID int64     `json:"question_id"`
    IsDraft    bool      `json:"is_draft"`
    Version    int       `json:"version"`
    CreatedAt  time.Time `json:"created_at"`
    UpdatedAt  time.Time `json:"updated_at"`
}

type ResponseBlock struct {
    ID         int64      `json:"id"`
    ResponseID int64      `json:"response_id"`
    Type       string     `json:"type"` // "text" | "photo" | "link"
    Content    *string    `json:"content"`
    FilePath   *string    `json:"file_path"`
    Caption    *string    `json:"caption"`
    LinkURL    *string    `json:"link_url"`
    SortOrder  int        `json:"sort_order"`
    CreatedAt  time.Time  `json:"created_at"`
    UpdatedAt  time.Time  `json:"updated_at"`
}

type Comment struct {
    ID         int64      `json:"id"`
    UserID     int64      `json:"user_id"`
    ResponseID int64      `json:"response_id"`
    ParentID   *int64     `json:"parent_id"`
    Body       string     `json:"body"`       // raw Markdown
    BodyHTML   string     `json:"body_html"`  // rendered at read time, not stored
    CreatedAt  time.Time  `json:"created_at"`
}

type Reaction struct {
    ID         int64     `json:"id"`
    UserID     int64     `json:"user_id"`
    ResponseID int64     `json:"response_id"`
    Emoji      string    `json:"emoji"`
    CreatedAt  time.Time `json:"created_at"`
}

type QuestionBank struct {
    ID        int64     `json:"id"`
    Text      string    `json:"text"`
    Category  string    `json:"category"`
    Used      bool      `json:"used"`
    CreatedAt time.Time `json:"created_at"`
}

type EmailLog struct {
    ID        int64      `json:"id"`
    UserID    *int64     `json:"user_id"`
    IssueID   *int64     `json:"issue_id"`
    Type      string     `json:"type"`
    Status    string     `json:"status"` // "pending" | "sent" | "failed"
    SentAt    *time.Time `json:"sent_at"`
    Error     *string    `json:"error"`
    CreatedAt time.Time  `json:"created_at"`
}

type NotificationPreferences struct {
    ID             int64     `json:"id"`
    UserID         int64     `json:"user_id"`
    IssueOpen      bool      `json:"issue_open"`
    Reminders      bool      `json:"reminders"`
    Published      bool      `json:"published"`
    CommentNotify  bool      `json:"comment_notify"`
    ReactionNotify bool      `json:"reaction_notify"`
    UpdatedAt      time.Time `json:"updated_at"`
}

type SchedulerEvent struct {
    ID          int64      `json:"id"`
    IssueID     *int64     `json:"issue_id"`
    EventType   string     `json:"event_type"`
    ScheduledAt time.Time  `json:"scheduled_at"`
    FiredAt     *time.Time `json:"fired_at"`
    WasLate     bool       `json:"was_late"`
    CreatedAt   time.Time  `json:"created_at"`
}

// --- API response wrappers ---

type ListResponse[T any] struct {
    Items   []T `json:"items"`
    Total   int `json:"total"`
    Page    int `json:"page"`
    PerPage int `json:"per_page"`
}

type ErrorResponse struct {
    Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
    Code    string            `json:"code"`
    Message string            `json:"message"`
    Fields  map[string]string `json:"fields,omitempty"`
}

// --- Composite types for templates ---

type ResponseWithBlocks struct {
    Response Response        `json:"response"`
    Blocks   []ResponseBlock `json:"blocks"`
    User     User            `json:"user"`
}

type IssueWithQuestions struct {
    Issue     Issue      `json:"issue"`
    Questions []Question `json:"questions"`
}

type CommentWithUser struct {
    Comment Comment `json:"comment"`
    User    User    `json:"user"`
}

type ReactionSummary struct {
    Emoji string `json:"emoji"`
    Count int    `json:"count"`
    Users []User `json:"users"`
    // Whether the current user has reacted with this emoji
    Reacted bool `json:"reacted"`
}

type SubmissionProgress struct {
    TotalMembers int                `json:"total_members"`
    Responded    int                `json:"responded"`
    Members      []MemberProgress   `json:"members"`
}

type MemberProgress struct {
    User      User `json:"user"`
    Responded bool `json:"responded"`
}
```

---

## 3. Database Layer (`internal/db/`)

### 3.1 Database Initialization (`db.go`)

```go
type DB struct {
    *sql.DB
}

func OpenDB(path string) (*DB, error)
```

**Steps:**
1. `sql.Open("sqlite", path)` — the `modernc.org/sqlite` driver registers as `"sqlite"`.
2. `db.SetMaxOpenConns(1)` — SQLite doesn't support true concurrent writes. A single write connection with WAL mode gives concurrent reads. **This is critical.** Without it, concurrent write attempts hit `SQLITE_BUSY` even with `busy_timeout`.
3. Execute PRAGMAs (must be done on each new connection, use `db.Exec`):
   ```sql
   PRAGMA journal_mode=WAL;
   PRAGMA foreign_keys=ON;
   PRAGMA busy_timeout=5000;
   PRAGMA synchronous=NORMAL;
   ```
4. Run migrations.
5. Seed admin user if not exists.
6. Seed settings row if not exists.
7. Seed question bank if not exists.

**Important detail about `SetMaxOpenConns(1)`**: WAL mode allows unlimited concurrent readers alongside one writer. With `database/sql`'s connection pool, setting max open conns to 1 means all operations serialize through one connection. This is safe and simple for <50 users. If we ever need concurrent reads while a write is pending, we'd need a separate read pool — but not for this scale.

**Alternative (better) approach**: Use two `*sql.DB` instances — one for writes (`SetMaxOpenConns(1)`) and one for reads (`SetMaxOpenConns(4)`). This gives concurrent read performance while serializing writes. Implement this from the start since it's trivial:

```go
type DB struct {
    write *sql.DB // max 1 connection
    read  *sql.DB // max 4 connections
}
```

All `SELECT` queries use `db.read`. All `INSERT/UPDATE/DELETE` queries use `db.write`. Transactions always use `db.write`.

### 3.2 Migration System

Embed migrations via `go:embed`:

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
```

**Migration runner logic:**

```go
func (db *DB) RunMigrations() error
```

1. Create `schema_migrations` table if not exists:
   ```sql
   CREATE TABLE IF NOT EXISTS schema_migrations (
       version INTEGER PRIMARY KEY,
       applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
   );
   ```
2. Read all `.sql` files from the embedded FS, sorted by filename (numeric prefix).
3. For each file, extract version number from filename (e.g., `001` from `001_initial.sql`).
4. Check if version exists in `schema_migrations`. If so, skip.
5. Begin transaction. Execute the SQL file contents. Insert version into `schema_migrations`. Commit.
6. Log each applied migration: `slog.Info("Applied migration", "version", version, "file", filename)`

### 3.3 Migration `001_initial.sql`

Contains the full schema from spec Section 3 — all CREATE TABLE statements exactly as specified. This is the only migration for v1.

### 3.4 Seeding

After migrations, in `OpenDB()`:

**Admin user seeding:**
```go
func (db *DB) SeedAdminUser(email string) error
```
1. Check if any user with `role = 'admin'` exists.
2. If not, insert: `INSERT INTO users (name, email, role, is_active) VALUES ('Admin', ?, 'admin', 1)`.
   - Name is "Admin" — the wizard Step 1 will update it.
3. Also create `notification_preferences` row with defaults for this user.

**Settings seeding:**
```go
func (db *DB) SeedSettings() error
```
1. `INSERT OR IGNORE INTO settings (id) VALUES (1)` — uses all column defaults.

**Question bank seeding:**
```go
func (db *DB) SeedQuestionBank() error
```
1. Check `SELECT COUNT(*) FROM question_bank`. If > 0, skip.
2. Read `seed/questions.json` (embedded via `go:embed`).
3. Parse JSON array of `{"text": "...", "category": "..."}`.
4. Bulk insert using a transaction: `INSERT OR IGNORE INTO question_bank (text, category) VALUES (?, ?)`.

### 3.5 Query Functions — Complete List

Each query file contains functions that accept `context.Context` as the first parameter.

#### `db/users.go`

```go
func (db *DB) GetUserByID(ctx context.Context, id int64) (*models.User, error)
func (db *DB) GetUserByEmail(ctx context.Context, email string) (*models.User, error)
func (db *DB) ListActiveUsers(ctx context.Context) ([]models.User, error)
func (db *DB) ListAllUsers(ctx context.Context) ([]models.User, error)  // for admin — includes deactivated
func (db *DB) CreateUser(ctx context.Context, name, email, role string) (int64, error)
func (db *DB) UpdateUser(ctx context.Context, id int64, name string, avatarURL, bio *string) error
func (db *DB) DeactivateUser(ctx context.Context, id int64) error  // sets is_active=0, deletes sessions
func (db *DB) ReactivateUser(ctx context.Context, id int64) error  // sets is_active=1
func (db *DB) SetUserName(ctx context.Context, id int64, name string) error
func (db *DB) PromoteToAdmin(ctx context.Context, id int64) error
```

**`DeactivateUser` must also delete all sessions for that user** (revocation on deactivation):
```sql
UPDATE users SET is_active = 0 WHERE id = ?;
DELETE FROM sessions WHERE user_id = ?;
```

#### `db/auth.go`

```go
// Tokens
func (db *DB) CreateAuthToken(ctx context.Context, userID int64, tokenHash, tokenType string, expiresAt time.Time) error
func (db *DB) GetAuthTokenByHash(ctx context.Context, tokenHash string) (*models.AuthToken, error)
func (db *DB) ConsumeAuthToken(ctx context.Context, id int64) error  // sets consumed_at = NOW()
func (db *DB) CleanupExpiredTokens(ctx context.Context) (int64, error)  // deletes consumed OR expired

// Sessions
func (db *DB) CreateSession(ctx context.Context, userID int64, tokenHash string, expiresAt time.Time) error
func (db *DB) GetSessionByHash(ctx context.Context, tokenHash string) (*models.Session, error)
func (db *DB) DeleteSession(ctx context.Context, id int64) error
func (db *DB) DeleteUserSessions(ctx context.Context, userID int64) error
func (db *DB) CleanupExpiredSessions(ctx context.Context) (int64, error)  // deletes expired
```

**`CleanupExpiredTokens` SQL:**
```sql
DELETE FROM auth_tokens WHERE consumed_at IS NOT NULL OR expires_at < CURRENT_TIMESTAMP;
```

**`CleanupExpiredSessions` SQL:**
```sql
DELETE FROM sessions WHERE expires_at < CURRENT_TIMESTAMP;
```

#### `db/settings.go`

```go
func (db *DB) GetSettings(ctx context.Context) (*models.Settings, error)
func (db *DB) UpdateSettings(ctx context.Context, s *models.Settings) error
func (db *DB) CompleteSetup(ctx context.Context) error  // sets setup_complete = 1
func (db *DB) IsSetupComplete(ctx context.Context) (bool, error)
```

#### `db/issues.go`

```go
func (db *DB) CreateIssue(ctx context.Context, title *string, month, year int, opensAt, deadline time.Time) (int64, error)
func (db *DB) GetIssueByID(ctx context.Context, id int64) (*models.Issue, error)
func (db *DB) GetIssueByMonthYear(ctx context.Context, month, year int) (*models.Issue, error)
func (db *DB) GetActiveIssue(ctx context.Context) (*models.Issue, error)  // status IN ('draft','collecting')
func (db *DB) ListIssues(ctx context.Context, status *string) ([]models.Issue, error)
func (db *DB) UpdateIssue(ctx context.Context, id int64, title *string, deadline *time.Time) error
func (db *DB) SetIssueStatus(ctx context.Context, id int64, status string) error
func (db *DB) PublishIssue(ctx context.Context, id int64) error  // sets status='published', published_at=NOW()
func (db *DB) HasActiveIssue(ctx context.Context) (bool, error)  // for "one active issue" constraint
```

**`GetActiveIssue` SQL:**
```sql
SELECT * FROM issues WHERE status IN ('draft', 'collecting') LIMIT 1;
```

**`HasActiveIssue` — used before creating a new issue to enforce the constraint:**
```sql
SELECT EXISTS(SELECT 1 FROM issues WHERE status IN ('draft', 'collecting'));
```

#### `db/questions.go`

```go
// Issue questions
func (db *DB) CreateQuestion(ctx context.Context, issueID int64, text string, category *string, source string, submittedBy *int64, sortOrder int) (int64, error)
func (db *DB) GetQuestionByID(ctx context.Context, id int64) (*models.Question, error)
func (db *DB) ListQuestionsByIssue(ctx context.Context, issueID int64) ([]models.Question, error)
func (db *DB) UpdateQuestion(ctx context.Context, id int64, text string) error
func (db *DB) DeleteQuestion(ctx context.Context, id int64) error
func (db *DB) ReorderQuestions(ctx context.Context, issueID int64, orderedIDs []int64) error

// Question bank
func (db *DB) SelectRandomUnusedQuestions(ctx context.Context, count int, mixCategories bool) ([]models.QuestionBank, error)
func (db *DB) MarkBankQuestionUsed(ctx context.Context, id int64) error
func (db *DB) ListQuestionBank(ctx context.Context, category *string, used *bool, page, perPage int) ([]models.QuestionBank, int, error)
func (db *DB) CreateBankQuestion(ctx context.Context, text, category string) (int64, error)
func (db *DB) UpdateBankQuestion(ctx context.Context, id int64, text, category string) error
func (db *DB) DeleteBankQuestion(ctx context.Context, id int64) error
```

**`SelectRandomUnusedQuestions` — the question selection algorithm:**
```sql
-- Prefer unused, fall back to any if not enough unused
SELECT * FROM question_bank WHERE used = 0 ORDER BY RANDOM() LIMIT ?;
-- If not enough results, fill remaining with:
SELECT * FROM question_bank ORDER BY RANDOM() LIMIT ?;
```

The function should try to get one question from each category for variety. Implementation:

```go
func (db *DB) SelectRandomUnusedQuestions(ctx context.Context, count int) ([]models.QuestionBank, error) {
    categories := []string{"life_updates", "deep_thoughts", "fun_silly", "memories", "goals", "recommendations", "hypotheticals"}
    var results []models.QuestionBank

    // Round 1: one unused question per category until we reach count
    for _, cat := range categories {
        if len(results) >= count {
            break
        }
        var q models.QuestionBank
        err := db.read.QueryRowContext(ctx,
            "SELECT id, text, category FROM question_bank WHERE used = 0 AND category = ? ORDER BY RANDOM() LIMIT 1", cat).
            Scan(&q.ID, &q.Text, &q.Category)
        if err == sql.ErrNoRows {
            continue
        }
        if err != nil {
            return nil, err
        }
        results = append(results, q)
    }

    // Round 2: if still under count, fill with random unused from any category
    if len(results) < count {
        // exclude already-selected IDs
        // ... fill remaining from unused, then from any
    }

    return results, nil
}
```

#### `db/responses.go`

```go
func (db *DB) CreateResponse(ctx context.Context, userID, questionID int64) (int64, error)
func (db *DB) GetResponseByID(ctx context.Context, id int64) (*models.Response, error)
func (db *DB) GetUserResponse(ctx context.Context, userID, questionID int64) (*models.Response, error)
func (db *DB) DeleteResponse(ctx context.Context, id int64) error
func (db *DB) SubmitResponse(ctx context.Context, id int64) error  // is_draft = 0
func (db *DB) AutosaveResponse(ctx context.Context, id int64, version int, blocks []models.ResponseBlock) error
func (db *DB) ListResponsesByIssue(ctx context.Context, issueID int64, onlySubmitted bool) ([]models.Response, error)
func (db *DB) ListUserResponsesForIssue(ctx context.Context, userID, issueID int64) ([]models.Response, error)
func (db *DB) GetSubmissionProgress(ctx context.Context, issueID int64) (*models.SubmissionProgress, error)
func (db *DB) CountPhotoBlocksForResponse(ctx context.Context, responseID int64) (int, error)

// Blocks
func (db *DB) CreateBlock(ctx context.Context, responseID int64, blockType string, content, filePath, caption, linkURL *string, sortOrder int) (int64, error)
func (db *DB) GetBlockByID(ctx context.Context, id int64) (*models.ResponseBlock, error)
func (db *DB) ListBlocksByResponse(ctx context.Context, responseID int64) ([]models.ResponseBlock, error)
func (db *DB) UpdateBlock(ctx context.Context, id int64, content, caption *string) error
func (db *DB) DeleteBlock(ctx context.Context, id int64) error
func (db *DB) ReorderBlocks(ctx context.Context, responseID int64, orderedIDs []int64) error
```

**`AutosaveResponse` — the critical autosave logic (single transaction):**

```go
func (db *DB) AutosaveResponse(ctx context.Context, id int64, expectedVersion int, blocks []models.ResponseBlock) (int, error) {
    tx, _ := db.write.BeginTx(ctx, nil)
    defer tx.Rollback()

    // 1. Check version matches
    var currentVersion int
    tx.QueryRowContext(ctx, "SELECT version FROM responses WHERE id = ?", id).Scan(&currentVersion)
    if currentVersion != expectedVersion {
        return currentVersion, ErrVersionConflict  // caller returns 409
    }

    // 2. Delete all existing blocks
    tx.ExecContext(ctx, "DELETE FROM response_blocks WHERE response_id = ?", id)

    // 3. Insert new blocks
    for i, b := range blocks {
        tx.ExecContext(ctx,
            "INSERT INTO response_blocks (response_id, type, content, file_path, caption, link_url, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)",
            id, b.Type, b.Content, b.FilePath, b.Caption, b.LinkURL, i)
    }

    // 4. Increment version + update timestamp
    tx.ExecContext(ctx, "UPDATE responses SET version = version + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?", id)

    tx.Commit()
    return currentVersion + 1, nil
}
```

**`GetSubmissionProgress` — who has responded:**

```sql
-- "responded" = has at least one non-draft response for any question in this issue
SELECT u.id, u.name, u.email, u.avatar_url,
    EXISTS(
        SELECT 1 FROM responses r
        JOIN questions q ON r.question_id = q.id
        WHERE q.issue_id = ? AND r.user_id = u.id AND r.is_draft = 0
    ) AS responded
FROM users u
WHERE u.is_active = 1 AND u.role IN ('admin', 'member');
```

**`ListResponsesByIssue` — responses for published view (JOIN through questions):**

```sql
SELECT r.* FROM responses r
JOIN questions q ON r.question_id = q.id
WHERE q.issue_id = ? AND r.is_draft = 0
ORDER BY q.sort_order, r.user_id;
```

#### `db/comments.go`

```go
func (db *DB) CreateComment(ctx context.Context, userID, responseID int64, parentID *int64, body string) (int64, error)
func (db *DB) GetCommentByID(ctx context.Context, id int64) (*models.Comment, error)
func (db *DB) ListCommentsByResponse(ctx context.Context, responseID int64) ([]models.CommentWithUser, error)
func (db *DB) DeleteComment(ctx context.Context, id int64) error
```

**`ListCommentsByResponse` — one-level threading:**
```sql
SELECT c.*, u.id as user_id, u.name, u.avatar_url
FROM comments c
JOIN users u ON c.user_id = u.id
WHERE c.response_id = ?
ORDER BY c.parent_id NULLS FIRST, c.created_at ASC;
```

In Go, organize into a tree: top-level comments (parent_id IS NULL) with their replies sorted by created_at.

#### `db/reactions.go`

```go
func (db *DB) ToggleReaction(ctx context.Context, userID, responseID int64, emoji string) (added bool, err error)
func (db *DB) ListReactionsByResponse(ctx context.Context, responseID int64) ([]models.ReactionSummary, error)
func (db *DB) GetReactionDigestForUser(ctx context.Context, userID int64, since time.Time) ([]models.Reaction, error)
```

**`ToggleReaction` — upsert/delete:**

```go
func (db *DB) ToggleReaction(ctx context.Context, userID, responseID int64, emoji string) (bool, error) {
    // Validate emoji is in allowed set
    allowed := map[string]bool{"❤️": true, "😂": true, "🔥": true, "👏": true, "🤔": true, "😢": true}
    if !allowed[emoji] {
        return false, ErrInvalidEmoji
    }

    // Try delete first
    result, _ := db.write.ExecContext(ctx,
        "DELETE FROM reactions WHERE user_id = ? AND response_id = ? AND emoji = ?",
        userID, responseID, emoji)
    rows, _ := result.RowsAffected()
    if rows > 0 {
        return false, nil // removed
    }
    // Insert
    db.write.ExecContext(ctx,
        "INSERT INTO reactions (user_id, response_id, emoji) VALUES (?, ?, ?)",
        userID, responseID, emoji)
    return true, nil // added
}
```

**`ListReactionsByResponse` — aggregated:**
```sql
SELECT emoji, COUNT(*) as count FROM reactions WHERE response_id = ? GROUP BY emoji;
-- Plus: for each emoji, get the user names:
SELECT u.id, u.name FROM reactions r JOIN users u ON r.user_id = u.id WHERE r.response_id = ? AND r.emoji = ?;
```

#### `db/email_log.go`

```go
func (db *DB) LogEmail(ctx context.Context, userID *int64, issueID *int64, emailType, status string, error *string) (int64, error)
func (db *DB) UpdateEmailLog(ctx context.Context, id int64, status string, sentAt *time.Time, error *string) error
func (db *DB) ListEmailLogs(ctx context.Context, page, perPage int) ([]models.EmailLog, int, error)
func (db *DB) GetEmailLogByID(ctx context.Context, id int64) (*models.EmailLog, error)
```

#### `db/notifications.go`

```go
func (db *DB) GetNotificationPreferences(ctx context.Context, userID int64) (*models.NotificationPreferences, error)
func (db *DB) UpsertNotificationPreferences(ctx context.Context, prefs *models.NotificationPreferences) error
func (db *DB) EnsureNotificationPreferences(ctx context.Context, userID int64) error  // creates default row if not exists
```

**`EnsureNotificationPreferences`** — called when a new user is created:
```sql
INSERT OR IGNORE INTO notification_preferences (user_id) VALUES (?);
```

#### `db/scheduler.go`

```go
func (db *DB) CreateSchedulerEvent(ctx context.Context, issueID *int64, eventType string, scheduledAt time.Time) error
func (db *DB) GetPendingEvents(ctx context.Context) ([]models.SchedulerEvent, error)  // fired_at IS NULL
func (db *DB) GetOverdueEvents(ctx context.Context) ([]models.SchedulerEvent, error)  // fired_at IS NULL AND scheduled_at < NOW()
func (db *DB) MarkEventFired(ctx context.Context, id int64, wasLate bool) error
func (db *DB) DeleteEventsForIssue(ctx context.Context, issueID int64) error  // used when extending deadline (reschedule)
func (db *DB) GetNextPendingEvent(ctx context.Context) (*models.SchedulerEvent, error)
```

**`GetOverdueEvents` — for startup recovery:**
```sql
SELECT * FROM scheduler_events
WHERE fired_at IS NULL AND scheduled_at <= CURRENT_TIMESTAMP
ORDER BY scheduled_at ASC;
```

---

## 4. Middleware (`internal/server/middleware.go`)

### 4.1 Middleware Chain Order

Applied to the mux in this order (outermost first):

1. **Recovery** — catches panics in handlers, logs stack trace, returns 500
2. **Security Headers** — sets CSP, X-Content-Type-Options, X-Frame-Options on all responses
3. **Request Logging** — logs method, path, status, duration, user_id
4. **CSRF** — sets cookie on GET, validates header on POST/PATCH/PUT/DELETE

Then per-route:
- **Auth** — validates session cookie, injects user into context
- **Admin** — checks user.Role == "admin"

### 4.2 Recovery Middleware

```go
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        defer func() {
            if err := recover(); err != nil {
                slog.Error("Panic recovered", "error", err, "stack", string(debug.Stack()))
                http.Error(w, "Internal Server Error", http.StatusInternalServerError)
            }
        }()
        next.ServeHTTP(w, r)
    })
}
```

### 4.3 Security Headers Middleware

```go
func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("X-Frame-Options", "DENY")
        w.Header().Set("Content-Security-Policy",
            "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'")
        next.ServeHTTP(w, r)
    })
}
```

**Note:** `'unsafe-inline'` for styles is needed because oat.ink may use inline styles, and email template previews embed inline CSS. The CSP prevents script injection, which is the main concern with uploaded SVG/HTML.

### 4.4 Request Logging Middleware

```go
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        ww := &responseWriter{ResponseWriter: w, statusCode: 200}
        next.ServeHTTP(ww, r)

        userID := int64(0)
        if u := UserFromContext(r.Context()); u != nil {
            userID = u.ID
        }

        slog.Info("Request",
            "method", r.Method,
            "path", r.URL.Path,
            "status", ww.statusCode,
            "duration_ms", time.Since(start).Milliseconds(),
            "user_id", userID,
        )
    })
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
    http.ResponseWriter
    statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
    rw.statusCode = code
    rw.ResponseWriter.WriteHeader(code)
}
```

### 4.5 CSRF Middleware

**Signed double-submit cookie pattern:**

The cookie value is `<nonce>.<signature>` where `nonce` is 16 random bytes
(hex) and `signature` is `base64url(HMAC-SHA256(SESSION_SECRET, nonce))`.
The header echoes the cookie verbatim (double-submit). On state-changing
requests we require both halves to match AND the cookie's signature to
validate against `SESSION_SECRET` — verified during the cookie-issuance
check, which re-mints the cookie if the existing one is missing or has
an invalid signature (self-healing on secret rotation).

```go
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        cookie, err := r.Cookie("csrf_token")
        if err != nil || !auth.ValidateSignedCSRFToken(s.config.SessionSecret, cookie.Value) {
            token := auth.GenerateSignedCSRFToken(s.config.SessionSecret)
            http.SetCookie(w, &http.Cookie{
                Name:     "csrf_token",
                Value:    token,
                Path:     "/",
                HttpOnly: false,  // JS must be able to read it
                Secure:   !s.config.DevMode,
                SameSite: http.SameSiteLaxMode,
                MaxAge:   86400 * 30, // 30 days
            })
            cookie = &http.Cookie{Name: "csrf_token", Value: token}
        }

        // For state-changing methods, validate the double-submit equality.
        if r.Method == "POST" || r.Method == "PATCH" || r.Method == "PUT" || r.Method == "DELETE" {
            header := r.Header.Get("X-CSRF-Token")
            if header == "" || header != cookie.Value {
                writeError(w, http.StatusForbidden, "forbidden", "CSRF token mismatch")
                return
            }
        }

        next.ServeHTTP(w, r)
    })
}
```

**Exempt from CSRF:** The `/auth/verify` GET route (magic link click from email), and `/health`.

### 4.6 Auth Middleware

```go
type contextKey string
const userContextKey contextKey = "user"

func (s *Server) authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        cookie, err := r.Cookie("session")
        if err != nil {
            // For page routes: redirect to /login
            // For API routes: return 401
            if strings.HasPrefix(r.URL.Path, "/api/") {
                writeError(w, http.StatusUnauthorized, "unauthorized", "Not logged in")
            } else {
                http.Redirect(w, r, "/login", http.StatusSeeOther)
            }
            return
        }

        tokenHash := sha256Hex(cookie.Value)
        session, err := s.db.GetSessionByHash(r.Context(), tokenHash)
        if err != nil || session.ExpiresAt.Before(time.Now()) {
            // Clear invalid cookie
            http.SetCookie(w, &http.Cookie{Name: "session", MaxAge: -1, Path: "/"})
            if strings.HasPrefix(r.URL.Path, "/api/") {
                writeError(w, http.StatusUnauthorized, "unauthorized", "Session expired")
            } else {
                http.Redirect(w, r, "/login", http.StatusSeeOther)
            }
            return
        }

        user, err := s.db.GetUserByID(r.Context(), session.UserID)
        if err != nil || !user.IsActive {
            // User deactivated
            http.SetCookie(w, &http.Cookie{Name: "session", MaxAge: -1, Path: "/"})
            http.Redirect(w, r, "/login", http.StatusSeeOther)
            return
        }

        ctx := context.WithValue(r.Context(), userContextKey, user)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func UserFromContext(ctx context.Context) *models.User {
    u, _ := ctx.Value(userContextKey).(*models.User)
    return u
}
```

### 4.7 Admin Middleware

```go
func (s *Server) adminMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        user := UserFromContext(r.Context())
        if user == nil || user.Role != "admin" {
            if strings.HasPrefix(r.URL.Path, "/api/") {
                writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
            } else {
                http.Redirect(w, r, "/", http.StatusSeeOther)
            }
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

---

## 5. Helper Functions (`internal/server/helpers.go`)

### 5.1 JSON Helpers

```go
func writeJSON(w http.ResponseWriter, status int, data any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
    writeJSON(w, status, models.ErrorResponse{
        Error: models.ErrorDetail{Code: code, Message: message},
    })
}

func writeValidationError(w http.ResponseWriter, fields map[string]string) {
    writeJSON(w, http.StatusBadRequest, models.ErrorResponse{
        Error: models.ErrorDetail{
            Code:    "validation_error",
            Message: "Invalid input",
            Fields:  fields,
        },
    })
}

func readJSON(r *http.Request, v any) error {
    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()
    return dec.Decode(v)
}
```

### 5.2 Pagination Helper

```go
type Pagination struct {
    Page    int
    PerPage int
    Offset  int
}

func parsePagination(r *http.Request) Pagination {
    page, _ := strconv.Atoi(r.URL.Query().Get("page"))
    perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
    if page < 1 { page = 1 }
    if perPage < 1 { perPage = 50 }
    if perPage > 100 { perPage = 100 }
    return Pagination{
        Page:    page,
        PerPage: perPage,
        Offset:  (page - 1) * perPage,
    }
}
```

### 5.3 Token Generation

```go
import "crypto/rand"
import "crypto/sha256"
import "encoding/hex"

func generateRandomToken(length int) (raw string, hash string, err error) {
    bytes := make([]byte, length)
    if _, err := rand.Read(bytes); err != nil {
        return "", "", err
    }
    raw = hex.EncodeToString(bytes)
    hash = sha256Hex(raw)
    return raw, hash, nil
}

func sha256Hex(s string) string {
    h := sha256.Sum256([]byte(s))
    return hex.EncodeToString(h[:])
}

func generateRandomString(length int) string {
    bytes := make([]byte, length)
    rand.Read(bytes)
    return hex.EncodeToString(bytes)
}
```

**Token sizes:**
- Login magic link: 32 bytes (64 hex chars)
- Email CTA token: 32 bytes
- Session token: 32 bytes
- CSRF token: 16 bytes (32 hex chars)

---

## 6. Server & Route Registration (`internal/server/`)

### 6.1 Server Struct

```go
type Server struct {
    db        *db.DB
    config    *config.Config
    emailer   *email.Sender
    scheduler *scheduler.Scheduler
    tmpl      *template.Template  // page templates
    emailTmpl *template.Template  // email templates
    mux       *http.ServeMux
    logger    *slog.Logger
}

func New(db *db.DB, cfg *config.Config, emailer *email.Sender, sched *scheduler.Scheduler) *Server
```

### 6.2 Route Registration (`routes.go`)

Using Go 1.22+ pattern matching (`GET /path`, `POST /path/{id}`):

```go
func (s *Server) RegisterRoutes() {
    // Public routes (no auth)
    s.mux.HandleFunc("GET /health", s.handleHealth)
    s.mux.HandleFunc("GET /login", s.handleLoginPage)
    s.mux.HandleFunc("GET /auth/verify", s.handleVerifyToken)
    s.mux.HandleFunc("POST /api/auth/login", s.handleRequestMagicLink)

    // Authenticated routes
    auth := s.authMiddleware
    admin := func(next http.HandlerFunc) http.Handler {
        return s.adminMiddleware(s.authMiddleware(next))
    }

    // Page routes
    s.mux.Handle("GET /", auth(http.HandlerFunc(s.handleLanding)))
    s.mux.Handle("GET /issues", auth(http.HandlerFunc(s.handleIssueArchive)))
    s.mux.Handle("GET /issues/{year}/{month}", auth(http.HandlerFunc(s.handleIssuePage)))
    s.mux.Handle("GET /issues/{id}/respond", auth(http.HandlerFunc(s.handleRespondPage)))
    s.mux.Handle("GET /albums", auth(http.HandlerFunc(s.handleAlbumsPage)))
    s.mux.Handle("GET /profile", auth(http.HandlerFunc(s.handleProfilePage)))

    // Admin page routes
    s.mux.Handle("GET /admin", admin(s.handleAdminDashboard))
    s.mux.Handle("GET /admin/members", admin(s.handleAdminMembers))
    s.mux.Handle("GET /admin/questions", admin(s.handleAdminQuestions))
    s.mux.Handle("GET /admin/settings", admin(s.handleAdminSettings))
    s.mux.Handle("GET /admin/setup", admin(s.handleAdminSetup))

    // File serving (auth required)
    s.mux.Handle("GET /uploads/", auth(http.HandlerFunc(s.handleUpload)))

    // API routes (auth required)
    s.mux.Handle("POST /api/auth/logout", auth(http.HandlerFunc(s.handleLogout)))
    s.mux.Handle("GET /api/auth/me", auth(http.HandlerFunc(s.handleMe)))

    s.mux.Handle("GET /api/users", auth(http.HandlerFunc(s.handleListUsers)))
    s.mux.Handle("PATCH /api/users/{id}", auth(http.HandlerFunc(s.handleUpdateUser)))
    s.mux.Handle("GET /api/users/{id}/preferences", auth(http.HandlerFunc(s.handleGetPreferences)))
    s.mux.Handle("PATCH /api/users/{id}/preferences", auth(http.HandlerFunc(s.handleUpdatePreferences)))

    // Admin-only API routes
    s.mux.Handle("POST /api/users/invite", admin(s.handleInviteUser))
    s.mux.Handle("POST /api/onboarding/complete", admin(s.handleCompleteOnboarding))
    s.mux.Handle("POST /api/issues", admin(s.handleCreateIssue))
    s.mux.Handle("PATCH /api/issues/{id}", admin(s.handleUpdateIssue))
    s.mux.Handle("POST /api/issues/{id}/publish", admin(s.handlePublishIssue))
    s.mux.Handle("POST /api/issues/{id}/extend", admin(s.handleExtendDeadline))
    s.mux.Handle("POST /api/issues/{id}/questions", admin(s.handleAddQuestion))
    s.mux.Handle("PATCH /api/questions/{id}", admin(s.handleEditQuestion))
    s.mux.Handle("DELETE /api/questions/{id}", admin(s.handleDeleteQuestion))
    s.mux.Handle("GET /api/admin/email-log", admin(s.handleEmailLog))
    s.mux.Handle("POST /api/admin/resend/{logId}", admin(s.handleResendEmail))
    s.mux.Handle("POST /api/admin/send-reminder/{issueId}", admin(s.handleSendReminder))
    s.mux.Handle("GET /api/admin/settings", admin(s.handleGetSettings))
    s.mux.Handle("PATCH /api/admin/settings", admin(s.handleUpdateSettings))
    s.mux.Handle("GET /api/question-bank", admin(s.handleListQuestionBank))
    s.mux.Handle("POST /api/question-bank", admin(s.handleCreateBankQuestion))
    s.mux.Handle("PATCH /api/question-bank/{id}", admin(s.handleEditBankQuestion))
    s.mux.Handle("DELETE /api/question-bank/{id}", admin(s.handleDeleteBankQuestion))

    // Member API routes (auth required)
    s.mux.Handle("GET /api/issues", auth(http.HandlerFunc(s.handleListIssues)))
    s.mux.Handle("GET /api/issues/{id}", auth(http.HandlerFunc(s.handleGetIssue)))
    s.mux.Handle("GET /api/issues/{id}/questions", auth(http.HandlerFunc(s.handleListQuestions)))
    s.mux.Handle("GET /api/issues/{id}/progress", auth(http.HandlerFunc(s.handleGetProgress)))
    s.mux.Handle("GET /api/issues/{id}/responses", auth(http.HandlerFunc(s.handleListResponses)))
    s.mux.Handle("GET /api/issues/{id}/responses/mine", auth(http.HandlerFunc(s.handleListMyResponses)))
    s.mux.Handle("POST /api/responses", auth(http.HandlerFunc(s.handleCreateResponse)))
    s.mux.Handle("DELETE /api/responses/{id}", auth(http.HandlerFunc(s.handleDeleteResponse)))
    s.mux.Handle("POST /api/responses/{id}/submit", auth(http.HandlerFunc(s.handleSubmitResponse)))
    s.mux.Handle("GET /api/responses/{id}/blocks", auth(http.HandlerFunc(s.handleListBlocks)))
    s.mux.Handle("POST /api/responses/{id}/blocks", auth(http.HandlerFunc(s.handleAddBlock)))
    s.mux.Handle("PATCH /api/blocks/{id}", auth(http.HandlerFunc(s.handleUpdateBlock)))
    s.mux.Handle("DELETE /api/blocks/{id}", auth(http.HandlerFunc(s.handleDeleteBlock)))
    s.mux.Handle("POST /api/responses/{id}/blocks/reorder", auth(http.HandlerFunc(s.handleReorderBlocks)))
    s.mux.Handle("POST /api/responses/{id}/blocks/upload", auth(http.HandlerFunc(s.handleUploadPhoto)))
    s.mux.Handle("PUT /api/responses/{id}/autosave", auth(http.HandlerFunc(s.handleAutosave)))
    s.mux.Handle("GET /api/responses/{id}/comments", auth(http.HandlerFunc(s.handleListComments)))
    s.mux.Handle("POST /api/responses/{id}/comments", auth(http.HandlerFunc(s.handleAddComment)))
    s.mux.Handle("DELETE /api/comments/{id}", auth(http.HandlerFunc(s.handleDeleteComment)))
    s.mux.Handle("POST /api/responses/{id}/reactions", auth(http.HandlerFunc(s.handleToggleReaction)))
    s.mux.Handle("GET /api/responses/{id}/reactions", auth(http.HandlerFunc(s.handleListReactions)))
    s.mux.Handle("POST /api/questions/submit", auth(http.HandlerFunc(s.handleFriendSubmitQuestion)))
    s.mux.Handle("GET /api/albums", auth(http.HandlerFunc(s.handleListAlbums)))

    // Static files (embedded)
    s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
}
```

### 6.3 Static File Embedding

```go
//go:embed static
var staticFS embed.FS

// In dev mode, use os.DirFS("static") instead of the embedded FS
```

---

## 7. Template System

### 7.1 Template Loading

```go
//go:embed templates
var templatesFS embed.FS

func (s *Server) loadTemplates() error {
    funcMap := template.FuncMap{
        "formatDate":      formatDate,       // time.Time → "March 2026"
        "formatDateTime":  formatDateTime,   // time.Time → "Mar 1, 2026 at 3:00 PM"
        "formatRelative":  formatRelative,   // time.Time → "2 hours ago"
        "truncate":        truncate,         // string, int → truncated string
        "safeHTML":        safeHTML,         // string → template.HTML (for rendered markdown)
        "add":             func(a, b int) int { return a + b },
        "sub":             func(a, b int) int { return a - b },
        "mul":             func(a, b int) int { return a * b },
        "div":             func(a, b float64) float64 { return a / b },
        "percent":         func(a, b int) int { if b == 0 { return 0 }; return a * 100 / b },
        "seq":             seq,             // int → []int{0, 1, ..., n-1} (for loops)
        "contains":        strings.Contains,
        "hasPrefix":       strings.HasPrefix,
        "lower":           strings.ToLower,
        "upper":           strings.ToUpper,
        "join":            strings.Join,
        "letterAvatar":    letterAvatar,     // name → first letter uppercase
        "jsonMarshal":     jsonMarshal,      // any → template.JS (safe JSON for <script> tags)
        "categoryLabel":   categoryLabel,    // "fun_silly" → "Fun & Silly"
        "emojiLabel":      emojiLabel,       // "❤️" → "love"
        "dict":            dict,             // "key1", val1, "key2", val2 → map (for passing multiple values to partials)
    }

    var fs fs.FS
    if s.config.DevMode {
        fs = os.DirFS("templates")
    } else {
        fs = templatesFS
    }

    s.tmpl = template.Must(
        template.New("").Funcs(funcMap).ParseFS(fs, "templates/layouts/*.html", "templates/pages/*.html", "templates/pages/admin/*.html"),
    )
}
```

### 7.2 Template Function Implementations

```go
func formatDate(t time.Time, tz string) string {
    loc, err := time.LoadLocation(tz)
    if err != nil {
        loc = time.UTC
    }
    return t.In(loc).Format("January 2006")
}

func formatDateTime(t time.Time, tz string) string {
    loc, _ := time.LoadLocation(tz)
    return t.In(loc).Format("Jan 2, 2006 at 3:04 PM")
}

func formatRelative(t time.Time) string {
    d := time.Since(t)
    switch {
    case d < time.Minute:
        return "just now"
    case d < time.Hour:
        m := int(d.Minutes())
        if m == 1 { return "1 minute ago" }
        return fmt.Sprintf("%d minutes ago", m)
    case d < 24*time.Hour:
        h := int(d.Hours())
        if h == 1 { return "1 hour ago" }
        return fmt.Sprintf("%d hours ago", h)
    default:
        days := int(d.Hours() / 24)
        if days == 1 { return "1 day ago" }
        return fmt.Sprintf("%d days ago", days)
    }
}

func truncate(s string, n int) string {
    if len(s) <= n { return s }
    return s[:n] + "..."
}

func safeHTML(s string) template.HTML {
    return template.HTML(s)
}

func letterAvatar(name string) string {
    if len(name) == 0 { return "?" }
    return strings.ToUpper(name[:1])
}

func jsonMarshal(v any) template.JS {
    b, _ := json.Marshal(v)
    return template.JS(b)
}

func categoryLabel(cat string) string {
    labels := map[string]string{
        "life_updates":    "Life Updates",
        "deep_thoughts":   "Deep Thoughts",
        "fun_silly":       "Fun & Silly",
        "memories":        "Memories",
        "goals":           "Goals",
        "recommendations": "Recommendations",
        "hypotheticals":   "Hypotheticals",
    }
    if l, ok := labels[cat]; ok { return l }
    return cat
}

func dict(pairs ...any) map[string]any {
    m := make(map[string]any)
    for i := 0; i < len(pairs)-1; i += 2 {
        m[pairs[i].(string)] = pairs[i+1]
    }
    return m
}
```

### 7.3 Base Layout (`templates/layouts/base.html`)

```html
{{define "base"}}
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>{{block "title" .}}PiecesOfLife{{end}}</title>
    <link rel="stylesheet" href="/static/oat.css">
    <link rel="stylesheet" href="/static/app.css">
    {{block "head" .}}{{end}}
</head>
<body>
    {{block "nav" .}}
    <nav>
        <a href="/">{{.Settings.LoopName}}</a>
        {{if .User}}
            <a href="/issues">Archive</a>
            <a href="/albums">Photos</a>
            {{if eq .User.Role "admin"}}
                <a href="/admin">Admin</a>
            {{end}}
            <a href="/profile">{{.User.Name}}</a>
        {{end}}
    </nav>
    {{end}}

    <main>
        {{block "content" .}}{{end}}
    </main>

    <script src="/static/oat.js"></script>
    {{block "scripts" .}}{{end}}
</body>
</html>
{{end}}
```

### 7.4 Page Data Structs

Each page handler passes a struct to the template. All page data structs embed a common base:

```go
type PageData struct {
    User     *models.User
    Settings *models.Settings
    CSRFToken string  // from cookie, for embedding in JS
}

type LoginPageData struct {
    PageData
    Error string
    Email string // pre-filled on expired token redirect
}

type RespondPageData struct {
    PageData
    Issue     models.Issue
    Questions []models.Question
    Responses map[int64]*models.ResponseWithBlocks // keyed by question_id
    Progress  models.SubmissionProgress
}

type IssuePageData struct {
    PageData
    Issue     models.Issue
    Responses []models.ResponseWithBlocks
    Questions []models.Question
    View      string // "by_question" | "by_person"
    Layout    string // "list" | "grid"
}

type AdminDashboardData struct {
    PageData
    CurrentIssue *models.Issue
    Progress     *models.SubmissionProgress
    PastIssues   []models.Issue
    RecentEmails []models.EmailLog
}

type SetupWizardData struct {
    PageData
    SuggestedQuestions []models.QuestionBank
}
```

---

## 8. Auth Handlers (`internal/server/auth.go`)

### 8.1 `handleLoginPage` — `GET /login`

Renders `login.html`. If `?email=` query param exists, pre-fills the email field (used for expired token redirect). If `?expired=1`, shows "Your link expired — we've sent you a fresh one!" message.

### 8.2 `handleRequestMagicLink` — `POST /api/auth/login`

**Request body:** `{"email": "alex@example.com"}`

**Logic:**
1. Parse email from body. Validate format.
2. Look up user by email. If not found → still return 200 (don't reveal whether email exists). Log the attempt.
3. **Rate limit check:** Count recent tokens for this email in the last hour. If >= 3, return 429.
   ```sql
   SELECT COUNT(*) FROM auth_tokens
   WHERE user_id = (SELECT id FROM users WHERE email = ?)
   AND type = 'login'
   AND created_at > datetime('now', '-1 hour');
   ```
4. Generate token: `raw, hash := generateRandomToken(32)`
5. Insert into `auth_tokens`: type='login', expires_at = NOW() + 30 min.
6. Send email with link: `{BaseURL}/auth/verify?token={raw}`
7. Return 200 `{"message": "Check your email for a login link"}`

### 8.3 `handleVerifyToken` — `GET /auth/verify`

**Query params:** `?token=xxx` or `?auth=xxx` (email CTA tokens use `auth` param)

**Logic:**
1. Get token from `token` or `auth` query param.
2. Hash it: `hash = sha256Hex(token)`
3. Look up in `auth_tokens` by hash.
4. If not found → redirect to `/login?error=invalid`
5. If `consumed_at IS NOT NULL` → redirect to `/login?error=used`
6. If `expires_at < NOW()` → redirect to `/login?email={user.email}&expired=1` and auto-send a fresh login link.
7. Mark consumed: `UPDATE auth_tokens SET consumed_at = CURRENT_TIMESTAMP WHERE id = ?`
8. Create session: generate token, hash it, insert into `sessions` with 30-day expiry.
9. Set cookie:
   ```go
   http.SetCookie(w, &http.Cookie{
       Name:     "session",
       Value:    rawSessionToken,
       Path:     "/",
       HttpOnly: true,
       Secure:   true,
       SameSite: http.SameSiteLaxMode,
       MaxAge:   86400 * 30, // 30 days
   })
   ```
10. **Redirect logic:**
    - If token type is `login` → redirect to `/` (landing page handles further routing)
    - If token type is `email_cta` → redirect to the original URL (extract from the referrer or store the intended destination). **Implementation:** The CTA URL in the email is like `/issues/42/respond?auth=xxx`. After consuming the token, redirect to `/issues/42/respond` (strip the auth param).

**Stripping the auth param for redirect:**
```go
redirectURL := r.URL.Path // e.g., "/issues/42/respond" (from the full URL)
// Actually, auth/verify is a separate route. The email CTA links to /auth/verify?token=xxx&redirect=/issues/42/respond
// OR: the email links directly to /issues/42/respond?auth=xxx, and the auth middleware handles it.
```

**Better approach:** Handle `?auth=` in the auth middleware itself. If a request has `?auth=xxx`, validate and consume the token, create session, set cookie, then strip the `auth` param and redirect to the same URL. This way, the `GET /auth/verify` route is only for login magic links, and email CTAs are handled transparently.

```go
// In authMiddleware, before checking session cookie:
if authToken := r.URL.Query().Get("auth"); authToken != "" {
    // Validate and consume token
    // Create session, set cookie
    // Redirect to same URL without ?auth param
    q := r.URL.Query()
    q.Del("auth")
    r.URL.RawQuery = q.Encode()
    http.Redirect(w, r, r.URL.String(), http.StatusSeeOther)
    return
}
```

### 8.4 `handleLogout` — `POST /api/auth/logout`

1. Get session cookie.
2. Hash it, look up session.
3. Delete session row.
4. Clear cookie (MaxAge: -1).
5. Return 204.

### 8.5 `handleMe` — `GET /api/auth/me`

Return the current user as JSON (from context).

---

## 9. Onboarding Handler (`internal/server/onboarding.go`)

### 9.1 `handleAdminSetup` — `GET /admin/setup`

1. Check if `settings.setup_complete == true`. If so, redirect to `/admin`.
2. Render `setup.html` with `SetupWizardData`.
3. Pre-select 5 random unused questions from the bank (one per category, up to 5).

### 9.2 `handleCompleteOnboarding` — `POST /api/onboarding/complete`

**Request body:**
```json
{
    "admin_name": "Pari",
    "loop_name": "Chaos Crew",
    "tagline": "Monthly dispatches from the friend zone",
    "frequency": "monthly",
    "submission_window_days": 7,
    "start_datetime": "2026-03-01T10:00:00",
    "timezone": "Europe/Berlin",
    "questions": [
        {"text": "What's something...", "category": "life_updates", "bank_id": 42},
        {"text": "Custom question here", "category": null, "bank_id": null}
    ],
    "invite_emails": ["alex@example.com", "maya@example.com"],
    "invite_note": "Hey! Join our monthly thing..."
}
```

**Logic (single transaction where possible):**

1. **Validate** all required fields.
2. **Update admin name:** `UPDATE users SET name = ? WHERE id = ?`
3. **Update settings:**
   - Convert `start_datetime` from admin's timezone to UTC using `time.LoadLocation(timezone)`.
   - Write all fields to `settings` table.
4. **Mark bank questions as used:** For each question with a `bank_id`, set `used = true`.
5. **Create the first issue:**
   - Compute `opens_at` = the UTC start_datetime.
   - Compute `deadline` = opens_at + submission_window_days.
   - Insert issue with status = `collecting`, month/year from opens_at.
6. **Create questions for the issue:** Insert each question into `questions` table with the issue_id.
7. **Create scheduler events** for this issue:
   - `reminder_1`: opens_at + (submission_window_days / 2), rounded to nearest day
   - `reminder_2`: deadline - 2 days
   - `auto_close`: deadline
8. **Invite friends:** For each email:
   - Check if user exists. If active → skip (already a member). If deactivated → reactivate.
   - If new → create user with `name = email.split("@")[0]`, role='member', is_active=1.
   - Create `notification_preferences` row.
   - Generate email CTA token (30-day expiry).
   - Send invite email with the token.
   - Log in `email_log`.
9. **Send "Issue Open" email** to all active members (including newly invited ones).
10. **Mark setup complete:** `UPDATE settings SET setup_complete = 1`
11. **Notify scheduler** to pick up the new events.
12. Return 200 with `{"redirect": "/admin"}`.

**Timezone conversion detail:**
```go
loc, _ := time.LoadLocation(req.Timezone) // e.g., "Europe/Berlin"
localTime, _ := time.ParseInLocation("2006-01-02T15:04:05", req.StartDatetime, loc)
utcTime := localTime.UTC()
```

---

## 10. Key Handler Implementations

### 10.1 Photo Upload — `POST /api/responses/{id}/blocks/upload`

```go
func (s *Server) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
    user := UserFromContext(r.Context())
    responseID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)

    // 1. Verify response belongs to user
    resp, _ := s.db.GetResponseByID(r.Context(), responseID)
    if resp.UserID != user.ID {
        writeError(w, 403, "forbidden", "Not your response")
        return
    }

    // 2. Check photo count limit (10 per response)
    count, _ := s.db.CountPhotoBlocksForResponse(r.Context(), responseID)
    if count >= 10 {
        writeError(w, 400, "validation_error", "Maximum 10 photos per response")
        return
    }

    // 3. Parse multipart (limit to 50MB)
    r.Body = http.MaxBytesReader(w, r.Body, 50<<20) // 50MB
    if err := r.ParseMultipartForm(50 << 20); err != nil {
        writeError(w, 413, "too_large", "File too large (max 50MB)")
        return
    }

    file, header, err := r.FormFile("photo")
    if err != nil {
        writeError(w, 400, "validation_error", "No file provided")
        return
    }
    defer file.Close()

    // 4. Validate content type
    contentType := header.Header.Get("Content-Type")
    allowed := map[string]string{
        "image/jpeg": ".jpg",
        "image/png":  ".png",
        "image/webp": ".webp",
    }
    ext, ok := allowed[contentType]
    if !ok {
        writeError(w, 400, "validation_error", "Only JPEG, PNG, and WebP are allowed")
        return
    }

    // 5. Generate file path: /data/uploads/{year}/{month}/{random_id}_{original_name}
    now := time.Now().UTC()
    randomID := generateRandomString(8)
    safeName := sanitizeFilename(header.Filename)
    relPath := fmt.Sprintf("/uploads/%d/%02d/%s_%s%s",
        now.Year(), now.Month(), randomID, safeName, ext)
    absPath := filepath.Join(s.config.UploadPath, relPath[len("/uploads/"):])

    // 6. Create directory if needed
    os.MkdirAll(filepath.Dir(absPath), 0755)

    // 7. Write file
    dst, _ := os.Create(absPath)
    defer dst.Close()
    io.Copy(dst, file)

    // 8. Create photo block
    sortOrder := count // append at end
    blockID, _ := s.db.CreateBlock(r.Context(), responseID, "photo", nil, &relPath, nil, nil, sortOrder)

    writeJSON(w, 201, map[string]any{
        "id":        blockID,
        "file_path": relPath,
    })
}

func sanitizeFilename(name string) string {
    // Remove path components, keep only alphanumeric + hyphens + underscores
    name = filepath.Base(name)
    ext := filepath.Ext(name)
    name = strings.TrimSuffix(name, ext)
    // Replace non-alphanumeric with underscore
    var b strings.Builder
    for _, r := range name {
        if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
            b.WriteRune(r)
        } else {
            b.WriteRune('_')
        }
    }
    result := b.String()
    if result == "" {
        result = "upload"
    }
    return result
}
```

### 10.2 Authenticated File Serving — `GET /uploads/*`

```go
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
    // Auth middleware already validated session.
    // Serve the file from the upload directory.
    // The URL path is /uploads/2026/03/abc123_summit.jpg
    // Map to filesystem: {UploadPath}/2026/03/abc123_summit.jpg
    requestedPath := strings.TrimPrefix(r.URL.Path, "/uploads/")
    absPath := filepath.Join(s.config.UploadPath, filepath.Clean(requestedPath))

    // Prevent directory traversal
    if !strings.HasPrefix(absPath, s.config.UploadPath) {
        http.NotFound(w, r)
        return
    }

    http.ServeFile(w, r, absPath)
}
```

### 10.3 Autosave — `PUT /api/responses/{id}/autosave`

**Request body:**
```json
{
    "version": 3,
    "blocks": [
        {"type": "text", "content": "We hiked to the summit..."},
        {"type": "photo", "file_path": "/uploads/2026/03/abc123_summit.jpg", "caption": "The view"},
        {"type": "text", "content": "On the way down..."}
    ]
}
```

**Handler:**
1. Parse body.
2. Verify response belongs to user.
3. Call `db.AutosaveResponse(ctx, id, version, blocks)`.
4. If `ErrVersionConflict` → return 409 with current version in body.
5. Return 200 with `{"version": newVersion}`.

### 10.4 Publish Issue — `POST /api/issues/{id}/publish`

**Logic:**
1. Get issue. Verify status is `collecting`.
2. **Compile the newsletter:** Gather all submitted (non-draft) responses with their blocks, grouped by question.
3. **Render the published email template** with all content.
4. **Send to all active members** (respecting `notification_preferences.published`). For each:
   - Generate email CTA token (30-day expiry) with link to `/issues/{year}/{month}?auth=xxx`.
   - Send email.
   - Log in `email_log`.
5. **Update issue:** status = 'published', published_at = NOW().
6. **Mark scheduler events as fired** (if auto_close hasn't fired yet, mark it).
7. Return 200.

### 10.5 Comment with Notification — `POST /api/responses/{id}/comments`

**Request body:** `{"body": "Great photo!", "parent_id": null}`

**Logic:**
1. Validate body not empty.
2. Create comment.
3. **Render markdown:** Use goldmark to convert body to HTML for the response.
4. **Send notification email** to the response author (if different from commenter, and if `comment_notify` preference is true):
   - Generate email CTA token.
   - Send "[Name] commented on your answer" email.
   - Log in `email_log`.
5. Return 201 with the created comment (including rendered HTML).

### 10.6 Landing Page — `GET /`

**Logic:**
1. If not setup complete → redirect to `/admin/setup`.
2. Get active issue.
3. If active issue in `collecting` state → redirect to `/issues/{id}/respond`.
4. If latest published issue exists → redirect to `/issues/{year}/{month}`.
5. Otherwise → redirect to `/issues` (archive).

---

## 11. Email System (`internal/email/`)

### 11.1 Sender (`sender.go`)

```go
type Sender struct {
    host     string
    port     int
    user     string
    pass     string
    from     string
    tls      string  // "implicit"
    baseURL  string
    db       *db.DB  // for logging
}

func NewSender(cfg *config.Config, db *db.DB) *Sender

func (s *Sender) Send(ctx context.Context, to, subject, htmlBody string) error
```

**SMTP implementation using `go-mail`:**

```go
func (s *Sender) Send(ctx context.Context, to, subject, htmlBody string) error {
    m := mail.NewMsg()
    m.From(s.from)
    m.To(to)
    m.Subject(subject)
    m.SetBodyString(mail.TypeTextHTML, htmlBody)

    c, err := mail.NewClient(s.host,
        mail.WithPort(s.port),
        mail.WithSMTPAuth(mail.SMTPAuthPlain),
        mail.WithUsername(s.user),
        mail.WithPassword(s.pass),
        mail.WithSSL(), // implicit TLS for port 465
    )
    if err != nil {
        return fmt.Errorf("creating SMTP client: %w", err)
    }
    return c.DialAndSendWithContext(ctx, m)
}
```

### 11.2 Batch Send Helper

```go
func (s *Sender) SendBatch(ctx context.Context, recipients []BatchRecipient) {
    for _, r := range recipients {
        logID, _ := s.db.LogEmail(ctx, &r.UserID, r.IssueID, r.EmailType, "pending", nil)
        err := s.Send(ctx, r.Email, r.Subject, r.HTMLBody)
        if err != nil {
            errStr := err.Error()
            s.db.UpdateEmailLog(ctx, logID, "failed", nil, &errStr)
            slog.Warn("Email failed", "type", r.EmailType, "user_id", r.UserID, "error", err)
        } else {
            now := time.Now()
            s.db.UpdateEmailLog(ctx, logID, "sent", &now, nil)
            slog.Info("Email sent", "type", r.EmailType, "user_id", r.UserID)
        }
    }
}

type BatchRecipient struct {
    UserID    int64
    Email     string
    Subject   string
    HTMLBody  string
    IssueID   *int64
    EmailType string
}
```

### 11.3 Email Template Rendering (`templates.go`)

```go
//go:embed templates/email
var emailTemplatesFS embed.FS

type EmailRenderer struct {
    tmpl *template.Template
}

func NewEmailRenderer(devMode bool) *EmailRenderer

// Methods for each email type:
func (r *EmailRenderer) RenderInvite(data InviteEmailData) (subject string, html string, err error)
func (r *EmailRenderer) RenderIssueOpen(data IssueOpenEmailData) (string, string, error)
func (r *EmailRenderer) RenderReminder(data ReminderEmailData) (string, string, error)
func (r *EmailRenderer) RenderPublished(data PublishedEmailData) (string, string, error)
func (r *EmailRenderer) RenderComment(data CommentEmailData) (string, string, error)
func (r *EmailRenderer) RenderReactionDigest(data ReactionDigestEmailData) (string, string, error)
```

**Email data structs:**

```go
type InviteEmailData struct {
    LoopName   string
    InviteNote string
    AuthURL    string  // full URL with auth token
    AdminName  string
}

type IssueOpenEmailData struct {
    LoopName   string
    IssueTitle string
    Month      string  // "March 2026"
    Questions  []string
    AuthURL    string
    RecipientName string
}

type ReminderEmailData struct {
    LoopName      string
    IssueTitle    string
    Responded     int
    Total         int
    DaysLeft      int
    AuthURL       string
    RecipientName string
    IsSecond      bool  // changes tone: "last chance" vs "friendly nudge"
}

type PublishedEmailData struct {
    LoopName   string
    IssueTitle string
    Month      string
    // For each question: question text + list of response excerpts (truncated to 200 chars)
    Sections   []PublishedSection
    AuthURL    string
    RecipientName string
}

type PublishedSection struct {
    Question  string
    Responses []PublishedResponseExcerpt
}

type PublishedResponseExcerpt struct {
    UserName  string
    AvatarURL *string
    Excerpt   string // first 200 chars of first text block
    PhotoURL  *string // first photo file_path, if any
    HasMore   bool
}

type CommentEmailData struct {
    LoopName       string
    CommenterName  string
    QuestionText   string
    CommentExcerpt string
    AuthURL        string
    RecipientName  string
}

type ReactionDigestEmailData struct {
    LoopName      string
    ReactionCount int
    AuthURL       string
    RecipientName string
}
```

### 11.4 Base Email Template (`templates/email/base.html`)

```html
{{define "email_base"}}
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{{block "email_title" .}}{{.LoopName}}{{end}}</title>
    <!--[if mso]><noscript><xml><o:OfficeDocumentSettings><o:PixelsPerInch>96</o:PixelsPerInch></o:OfficeDocumentSettings></xml></noscript><![endif]-->
    <style>
        @media (prefers-color-scheme: dark) {
            body { background-color: #1a1a1a !important; }
            .email-container { background-color: #2d2d2d !important; }
            .email-body { color: #e0e0e0 !important; }
            h1, h2, h3 { color: #ffffff !important; }
            .cta-button { background-color: #4CAF50 !important; }
        }
    </style>
</head>
<body style="margin:0;padding:0;background-color:#f5f5f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Oxygen,Ubuntu,sans-serif;">
<table role="presentation" cellspacing="0" cellpadding="0" border="0" width="100%">
<tr><td style="padding:20px 0;">
<table class="email-container" role="presentation" cellspacing="0" cellpadding="0" border="0" width="600" style="margin:0 auto;background-color:#ffffff;border-radius:8px;overflow:hidden;">

    <!-- Header -->
    <tr><td style="padding:24px 32px;background-color:#2d5016;color:#ffffff;">
        <h1 style="margin:0;font-size:20px;font-weight:600;">{{.LoopName}}</h1>
    </td></tr>

    <!-- Body -->
    <tr><td class="email-body" style="padding:32px;color:#333333;font-size:16px;line-height:1.6;">
        {{block "email_content" .}}{{end}}
    </td></tr>

    <!-- Footer -->
    <tr><td style="padding:16px 32px;background-color:#f9f9f9;color:#999999;font-size:12px;text-align:center;">
        Sent with {{.LoopName}}
    </td></tr>

</table>
</td></tr>
</table>
</body>
</html>
{{end}}
```

**CTA button style (reused across templates):**
```html
{{define "cta_button"}}
<table role="presentation" cellspacing="0" cellpadding="0" border="0" style="margin:24px auto;">
<tr><td class="cta-button" style="background-color:#2d5016;border-radius:6px;">
    <a href="{{.URL}}" style="display:inline-block;padding:14px 32px;color:#ffffff;text-decoration:none;font-weight:600;font-size:16px;">
        {{.Label}}
    </a>
</td></tr>
</table>
{{end}}
```

---

## 12. Scheduler (`internal/scheduler/scheduler.go`)

### 12.1 Scheduler Struct

```go
type Scheduler struct {
    db      *db.DB
    emailer *email.Sender
    renderer *email.EmailRenderer
    config  *config.Config
    cancel  context.CancelFunc
    wg      sync.WaitGroup
    alive   atomic.Bool  // for health check
}

func New(db *db.DB, emailer *email.Sender, renderer *email.EmailRenderer, cfg *config.Config) *Scheduler
func (s *Scheduler) Start(ctx context.Context)
func (s *Scheduler) Stop()
func (s *Scheduler) IsAlive() bool
func (s *Scheduler) ScheduleIssueEvents(ctx context.Context, issue *models.Issue) error
func (s *Scheduler) RescheduleIssueEvents(ctx context.Context, issue *models.Issue) error
```

### 12.2 Start Logic

```go
func (s *Scheduler) Start(ctx context.Context) {
    ctx, s.cancel = context.WithCancel(ctx)
    s.alive.Store(true)

    s.wg.Add(1)
    go func() {
        defer s.wg.Done()
        defer s.alive.Store(false)

        // 1. Fire any overdue events (startup recovery)
        s.fireOverdueEvents(ctx)

        // 2. Schedule daily cleanup events (token + session cleanup)
        s.scheduleDailyCleanup(ctx)

        // 3. Main loop: check for pending events every 60 seconds
        ticker := time.NewTicker(60 * time.Second)
        defer ticker.Stop()

        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                s.alive.Store(true)
                s.checkAndFireEvents(ctx)
            }
        }
    }()
}
```

### 12.3 Event Firing

```go
func (s *Scheduler) checkAndFireEvents(ctx context.Context) {
    events, _ := s.db.GetOverdueEvents(ctx)
    for _, e := range events {
        s.fireEvent(ctx, e, false)
    }
}

func (s *Scheduler) fireOverdueEvents(ctx context.Context) {
    events, _ := s.db.GetOverdueEvents(ctx)
    for _, e := range events {
        slog.Info("Firing late event",
            "event_type", e.EventType,
            "issue_id", e.IssueID,
            "scheduled_at", e.ScheduledAt,
            "delay", time.Since(e.ScheduledAt),
        )
        s.fireEvent(ctx, e, true)
    }
}

func (s *Scheduler) fireEvent(ctx context.Context, e models.SchedulerEvent, wasLate bool) {
    var err error
    switch e.EventType {
    case "reminder_1":
        err = s.sendReminder(ctx, *e.IssueID, false)
    case "reminder_2":
        err = s.sendReminder(ctx, *e.IssueID, true)
    case "auto_close":
        err = s.autoPublish(ctx, *e.IssueID)
    case "reaction_digest":
        err = s.sendReactionDigests(ctx)
    case "token_cleanup":
        err = s.cleanupTokens(ctx)
    case "session_cleanup":
        err = s.cleanupSessions(ctx)
    }

    if err != nil {
        slog.Error("Event failed", "event_type", e.EventType, "error", err)
        return
    }

    s.db.MarkEventFired(ctx, e.ID, wasLate)
}
```

### 12.4 Specific Event Handlers

**`sendReminder`:**
1. Get issue by ID.
2. Get submission progress.
3. For reminder_2: only target non-responders.
4. For each target user: check `notification_preferences.reminders`, generate auth token, render reminder email, send.

**`autoPublish`:**
1. Same logic as `handlePublishIssue` handler but called by the scheduler.
2. Shared via a service function that both the handler and scheduler call.

**`sendReactionDigests`:**
1. For each active user: query reactions on their responses since the last digest.
2. If any reactions exist and `reaction_notify` preference is true: render digest email, send.

**`cleanupTokens`:**
```go
count, _ := s.db.CleanupExpiredTokens(ctx)
slog.Info("Token cleanup", "deleted", count)
```

**`cleanupSessions`:**
```go
count, _ := s.db.CleanupExpiredSessions(ctx)
slog.Info("Session cleanup", "deleted", count)
```

### 12.5 Scheduling Events for a New Issue

```go
func (s *Scheduler) ScheduleIssueEvents(ctx context.Context, issue *models.Issue) error {
    // Reminder 1: halfway through submission window
    reminder1At := issue.OpensAt.Add(issue.Deadline.Sub(issue.OpensAt) / 2)
    s.db.CreateSchedulerEvent(ctx, &issue.ID, "reminder_1", reminder1At)

    // Reminder 2: 2 days before deadline
    reminder2At := issue.Deadline.Add(-48 * time.Hour)
    if reminder2At.Before(reminder1At) {
        // Short window — skip second reminder
    } else {
        s.db.CreateSchedulerEvent(ctx, &issue.ID, "reminder_2", reminder2At)
    }

    // Auto-close: at deadline
    s.db.CreateSchedulerEvent(ctx, &issue.ID, "auto_close", issue.Deadline)

    return nil
}
```

### 12.6 Daily Cleanup Scheduling

On startup, schedule daily cleanup events for the next 24 hours:
```go
func (s *Scheduler) scheduleDailyCleanup(ctx context.Context) {
    now := time.Now().UTC()
    // Schedule for next midnight UTC
    nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

    s.db.CreateSchedulerEvent(ctx, nil, "token_cleanup", nextMidnight)
    s.db.CreateSchedulerEvent(ctx, nil, "session_cleanup", nextMidnight)
    s.db.CreateSchedulerEvent(ctx, nil, "reaction_digest", nextMidnight)
}
```

The `UNIQUE(issue_id, event_type, scheduled_at)` constraint prevents duplicates if the app restarts multiple times in a day (issue_id is NULL for global events, and the scheduled_at is the same midnight).

---

## 13. Markdown Rendering (for Comments)

```go
import (
    "github.com/yuin/goldmark"
    "bytes"
)

var md = goldmark.New()

func renderMarkdown(source string) string {
    var buf bytes.Buffer
    if err := md.Convert([]byte(source), &buf); err != nil {
        return template.HTMLEscapeString(source)
    }
    return buf.String()
}
```

goldmark's default configuration already strips raw HTML (safe by default). No need for `WithUnsafe(false)` — that's the default. The rendered output is inserted into templates as `template.HTML`.

---

## 14. Static Assets (`static/`)

### 14.1 `editor.js` — Block Editor (~250 lines)

**Core responsibilities:**
1. **Auto-save**: `setInterval` every 30 seconds + `blur` event
2. **Block insertion**: add text/photo/link blocks
3. **Photo upload**: `FormData` + `fetch`
4. **Link detection**: paste event → regex match
5. **Drag-to-reorder**: SortableJS initialization
6. **Unsaved changes tracking**: `beforeunload`

**Structure:**

```javascript
(function() {
    'use strict';

    const AUTOSAVE_INTERVAL = 30000; // 30 seconds
    const MAX_FILE_SIZE = 50 * 1024 * 1024; // 50MB
    const CSRF_TOKEN = getCookie('csrf_token');

    let dirty = false; // tracks unsaved changes
    let currentVersion = parseInt(document.getElementById('editor').dataset.version);

    // --- CSRF helper ---
    function getCookie(name) {
        const match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'));
        return match ? match[2] : '';
    }

    function apiHeaders() {
        return {
            'Content-Type': 'application/json',
            'X-CSRF-Token': CSRF_TOKEN,
        };
    }

    // --- Autosave ---
    function collectBlocks(questionId) {
        const container = document.querySelector(`[data-question-id="${questionId}"] .blocks`);
        const blocks = [];
        container.querySelectorAll('.block').forEach((el, i) => {
            const type = el.dataset.type;
            const block = { type, sort_order: i };
            if (type === 'text') {
                block.content = el.querySelector('textarea').value;
            } else if (type === 'photo') {
                block.file_path = el.dataset.filePath;
                block.caption = el.querySelector('.caption')?.value || null;
            } else if (type === 'link') {
                block.link_url = el.dataset.linkUrl;
                block.content = el.querySelector('.link-text')?.value || null;
            }
            blocks.push(block);
        });
        return blocks;
    }

    async function autosave(responseId) {
        if (!dirty) return;

        const blocks = collectBlocks(responseId);
        try {
            const res = await fetch(`/api/responses/${responseId}/autosave`, {
                method: 'PUT',
                headers: apiHeaders(),
                body: JSON.stringify({ version: currentVersion, blocks }),
            });

            if (res.status === 409) {
                // Version conflict — reload
                const data = await res.json();
                location.reload();
                return;
            }

            if (res.ok) {
                const data = await res.json();
                currentVersion = data.version;
                dirty = false;
                updateSaveIndicator('auto-saved just now');
            }
        } catch (e) {
            updateSaveIndicator('save failed — will retry');
        }
    }

    // --- Photo upload ---
    async function uploadPhoto(responseId, file) {
        if (file.size > MAX_FILE_SIZE) {
            showError('This file is too large (max 50MB)');
            return;
        }

        const formData = new FormData();
        formData.append('photo', file);

        const res = await fetch(`/api/responses/${responseId}/blocks/upload`, {
            method: 'POST',
            headers: { 'X-CSRF-Token': CSRF_TOKEN },
            body: formData,
        });

        if (res.status === 413) {
            showError('File too large (max 50MB)');
            return;
        }

        if (res.ok) {
            const data = await res.json();
            insertPhotoBlock(responseId, data.file_path, data.id);
            dirty = true;
        }
    }

    // --- Link detection ---
    function detectLink(text) {
        const patterns = [
            /https?:\/\/(open\.spotify\.com|spotify\.link)\S+/i,
            /https?:\/\/(www\.)?youtube\.com\/watch\S+/i,
            /https?:\/\/youtu\.be\/\S+/i,
        ];
        for (const p of patterns) {
            const match = text.match(p);
            if (match) return match[0];
        }
        return null;
    }

    // --- SortableJS init ---
    document.querySelectorAll('.blocks').forEach(container => {
        new Sortable(container, {
            animation: 150,
            handle: '.drag-handle',
            onEnd: () => { dirty = true; },
        });
    });

    // --- beforeunload ---
    window.addEventListener('beforeunload', (e) => {
        if (dirty) {
            e.preventDefault();
            e.returnValue = '';
        }
    });

    // --- Mark dirty on input ---
    document.addEventListener('input', (e) => {
        if (e.target.closest('.block')) {
            dirty = true;
        }
    });

    // --- Auto-save interval ---
    // Each response section has its own save loop
    document.querySelectorAll('[data-response-id]').forEach(section => {
        const responseId = section.dataset.responseId;
        setInterval(() => autosave(responseId), AUTOSAVE_INTERVAL);
    });

    // --- Save on blur ---
    window.addEventListener('blur', () => {
        document.querySelectorAll('[data-response-id]').forEach(section => {
            autosave(section.dataset.responseId);
        });
    });

})();
```

### 14.2 `app.css` — Custom Styles

Minimal custom CSS on top of oat.ink:

```css
/* Progress bar */
.progress-bar { height: 8px; background: #e0e0e0; border-radius: 4px; overflow: hidden; }
.progress-bar-fill { height: 100%; background: #2d5016; transition: width 0.3s; }

/* Save indicator */
.save-indicator { font-size: 0.85rem; color: #888; margin-top: 4px; }

/* Block editor */
.block { position: relative; margin-bottom: 1rem; }
.block .drag-handle { cursor: grab; opacity: 0.3; }
.block:hover .drag-handle { opacity: 1; }
.block textarea { width: 100%; min-height: 80px; resize: vertical; }
.block-toolbar { display: flex; gap: 0.5rem; padding: 0.5rem; border-top: 1px solid #eee; }

/* Photo block */
.photo-block img { max-width: 100%; border-radius: 4px; }
.photo-block .caption { width: 100%; font-size: 0.9rem; margin-top: 0.25rem; }

/* Reaction bar */
.reaction-bar { display: flex; gap: 0.5rem; align-items: center; }
.reaction-btn { cursor: pointer; padding: 4px 8px; border-radius: 999px; border: 1px solid #ddd; }
.reaction-btn.active { background: #f0f0f0; border-color: #999; }

/* Comment sidebar */
.comment-sidebar { position: fixed; right: 0; top: 0; width: 400px; height: 100vh; background: #fff; box-shadow: -2px 0 8px rgba(0,0,0,0.1); transform: translateX(100%); transition: transform 0.2s; z-index: 100; overflow-y: auto; padding: 1rem; }
.comment-sidebar.open { transform: translateX(0); }

/* Lightbox */
.lightbox { position: fixed; inset: 0; background: rgba(0,0,0,0.9); display: flex; align-items: center; justify-content: center; z-index: 200; }
.lightbox img { max-width: 90vw; max-height: 90vh; object-fit: contain; }

/* Drag-over state for photo drop */
.drop-target { border: 2px dashed #2d5016; border-radius: 4px; }

/* Letter avatar */
.letter-avatar { width: 32px; height: 32px; border-radius: 50%; background: #2d5016; color: #fff; display: flex; align-items: center; justify-content: center; font-weight: 600; font-size: 14px; }
```

### 14.3 Vendored Libraries

- **oat.css + oat.js**: Download from oat.ink, save directly into `static/`. These are small (~8KB total). No modification needed.
- **sortable.min.js**: Download SortableJS minified (~10KB) from CDN or npm, save into `static/`.

---

## 15. Main Entrypoint (`main.go`)

```go
func main() {
    // 1. Load config
    cfg, err := config.Load()
    if err != nil {
        slog.Error("Failed to load config", "error", err)
        os.Exit(1)
    }

    // 2. Set up logging
    var handler slog.Handler
    level := parseLogLevel(cfg.LogLevel)
    if cfg.LogFormat == "json" {
        handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
    } else {
        handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
    }
    slog.SetDefault(slog.New(handler))

    // 3. Open database
    database, err := db.OpenDB(cfg.DatabasePath)
    if err != nil {
        slog.Error("Failed to open database", "error", err)
        os.Exit(1)
    }
    database.SeedAdminUser(cfg.AdminEmail)
    database.SeedSettings()
    database.SeedQuestionBank()

    // 4. Create email sender
    emailer := email.NewSender(cfg, database)
    renderer := email.NewEmailRenderer(cfg.DevMode)

    // 5. Create and start scheduler
    sched := scheduler.New(database, emailer, renderer, cfg)

    // 6. Create HTTP server
    srv := server.New(database, cfg, emailer, sched)

    // 7. Start scheduler in background
    ctx, cancel := context.WithCancel(context.Background())
    sched.Start(ctx)

    // 8. Start HTTP server
    httpServer := &http.Server{
        Addr:    fmt.Sprintf(":%d", cfg.Port),
        Handler: srv.Handler(),
    }

    go func() {
        slog.Info("Server starting", "port", cfg.Port, "db_path", cfg.DatabasePath)
        if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
            slog.Error("Server error", "error", err)
            os.Exit(1)
        }
    }()

    // 9. Graceful shutdown
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
    sig := <-quit
    slog.Info("Shutting down...", "signal", sig.String())

    // Stop accepting new connections, drain in-flight (30s timeout)
    shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer shutdownCancel()

    if err := httpServer.Shutdown(shutdownCtx); err != nil {
        slog.Error("Forced shutdown", "error", err)
    }

    // Stop scheduler
    cancel()
    sched.Stop() // waits for wg.Wait()

    // Close database
    database.Close()

    slog.Info("Shutdown complete")
}
```

---

## 16. Health Check

```go
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
    dbOK := "ok"
    if err := s.db.Ping(r.Context()); err != nil {
        dbOK = "error"
    }

    schedulerOK := "ok"
    if !s.scheduler.IsAlive() {
        schedulerOK = "error"
    }

    status := http.StatusOK
    if dbOK != "ok" || schedulerOK != "ok" {
        status = http.StatusServiceUnavailable
    }

    writeJSON(w, status, map[string]string{
        "status":    map[bool]string{true: "ok", false: "degraded"}[dbOK == "ok" && schedulerOK == "ok"],
        "db":        dbOK,
        "scheduler": schedulerOK,
    })
}
```

`db.Ping`:
```go
func (db *DB) Ping(ctx context.Context) error {
    return db.read.PingContext(ctx)
}
```

---

## 17. Implementation Order (Phase 1a Detailed)

This is the recommended order to build Phase 1a, where each step builds on the previous:

1. **Project scaffold**: `go mod init`, create directory structure, add dependencies.
2. **Config**: `internal/config/config.go`.
3. **Models**: `internal/models/models.go` — all types.
4. **Database + migrations**: `internal/db/db.go`, `001_initial.sql`, PRAGMA setup, migration runner.
5. **Seeding**: admin user, settings, question bank.
6. **Template system**: embedding, `base.html`, template functions. Build a single test page to verify rendering works.
7. **Middleware**: recovery, security headers, logging. Wire up the server with a test route.
8. **Auth**: magic link flow end-to-end. Login page → request link → verify → session → redirect. Logout.
9. **CSRF middleware**: set cookie, validate header.
10. **Auth middleware**: session validation, context injection.
11. **Landing page**: redirect logic.
12. **Onboarding wizard**: setup page + complete handler. This is the critical path — creates first issue, invites friends.
13. **Email sending**: SMTP client, invite email template, issue open email template.
14. **Question bank**: selection algorithm, bank CRUD.
15. **Issue creation**: create issue + schedule events.
16. **Response editor**: respond page, create response, text blocks only, autosave.
17. **Submit flow**: mark responses as submitted.
18. **Published issue view**: compile responses, render page.
19. **Publish handler**: compile + send newsletter email.
20. **Admin dashboard**: basic version with current issue + progress.
21. **Docker**: Dockerfile, docker-compose.yml.
22. **Health check**: `/health` endpoint.
23. **Graceful shutdown**: signal handling in main.go.
24. **Structured logging**: verify all log points are present.

---

## 18. Critical Edge Cases & Gotchas

### 18.1 SQLite `CURRENT_TIMESTAMP` Format

SQLite's `CURRENT_TIMESTAMP` returns `YYYY-MM-DD HH:MM:SS` (no timezone indicator). Since the container runs in UTC and we never set TZ, this is fine. But when scanning into Go's `time.Time`, the `modernc.org/sqlite` driver may return a string. Use a custom scanner or parse with `time.Parse("2006-01-02 15:04:05", s)`.

**Recommended approach:** Use `_time_format=sqlite` connection string parameter with `modernc.org/sqlite` to auto-parse timestamps:
```go
sql.Open("sqlite", path+"?_time_format=sqlite")
```

### 18.2 Wizard Idempotency

The onboarding `POST /api/onboarding/complete` must be idempotent. If the admin submits the wizard twice (e.g., browser back button), the second call should not create duplicate issues or send duplicate emails. Check `settings.setup_complete` at the start and return early if already true.

### 18.3 Email CTA Token in Auth Middleware

When the auth middleware sees `?auth=xxx` on a URL, it must:
1. Consume the token.
2. Create a session.
3. Set the cookie.
4. **Strip the `auth` param from the URL and redirect.**

This is important because if the user bookmarks the URL with `?auth=xxx`, subsequent visits would try to re-consume an already-consumed token and fail. The redirect ensures the browser's address bar shows the clean URL.

### 18.4 Concurrent Autosave vs Submit

If autosave fires at the same moment the user clicks Submit:
- The autosave `PUT` arrives first, increments version from 3→4.
- The submit `POST /api/responses/{id}/submit` doesn't use version — it just sets `is_draft = false`.
- No conflict. But the user may have made changes between the last autosave and clicking submit.

**Solution:** The Submit button should trigger a final autosave (with the current version), wait for it to succeed, then call the submit endpoint. Do this in `editor.js`:
```javascript
async function submitAll() {
    // Autosave all dirty responses first
    for (const section of document.querySelectorAll('[data-response-id]')) {
        await autosave(section.dataset.responseId);
    }
    // Then submit
    const res = await fetch(`/api/responses/${responseId}/submit`, {
        method: 'POST', headers: apiHeaders()
    });
    if (res.ok) showToast("Your answers are in!");
}
```

### 18.5 File Path Security in Upload Serving

The `handleUpload` handler must prevent directory traversal. `filepath.Clean()` normalizes `..` segments, and the `strings.HasPrefix` check ensures the resolved path is within the upload directory. This is already handled in the implementation above.

### 18.6 Timezone Handling

- **Storage:** Always UTC. Every `time.Time` in the database is UTC.
- **Input:** Admin enters local time during wizard. Convert using `time.LoadLocation(settings.Timezone)`.
- **Display:** Template functions accept timezone string parameter and convert for display.
- **Scheduler:** All events are scheduled in UTC. The scheduler compares against `time.Now().UTC()`.

### 18.7 `modernc.org/sqlite` Driver Registration

The driver registers itself with the name `"sqlite"` via an `init()` function. Just import it with a blank identifier:
```go
import _ "modernc.org/sqlite"
```

### 18.8 Question Submission During Collecting

The `POST /api/questions/submit` handler must verify:
1. There is an active issue in `collecting` state.
2. The user is active.
3. The question text is non-empty.

Then insert with `source = 'friend'`, `submitted_by = user.ID`, `issue_id = active_issue.ID`, and `sort_order = MAX(sort_order) + 1` for that issue.

### 18.9 Publish Compilation

When publishing, the system must:
1. Gather all submitted responses (not drafts) for the issue.
2. For each question, get all responses with their blocks.
3. For the email: truncate text to 200 chars, include only the first photo.
4. For the web view: render everything.
5. Handle the case where some questions have zero responses (still show the question header).

### 18.10 Extending Deadline

When the admin extends a deadline:
1. Update `issues.deadline`.
2. Delete unfired scheduler events for this issue (`fired_at IS NULL`).
3. Recreate them with new times based on the new deadline.
4. The scheduler's next tick will pick up the new events.
