# Improvements

Findings from a full codebase review (June 2026), covering the HTTP layer, data layer,
scheduler/email/deployment, frontend, and tests. Organized as: bugs and hardening to fix,
then feature work to pick up next.

## Overall assessment

The fundamentals are solid: token hashes at rest, atomic single-use token consumption,
HMAC-signed double-submit CSRF, parameterized SQL throughout, optimistic concurrency on
autosave done correctly inside a transaction, and scheduler missed-event recovery with
UNIQUE-constraint dedup. Spec Phases 1–3 are essentially complete, and most of Phase 4 too.
What remains is a layer of operational sharp edges and a handful of real bugs.

---

## Part 1 — Fixes

### High priority

#### 1. Insecure-by-default deployment
- `docker-compose.yml` ships `DEV_MODE: "true"` and `SESSION_SECRET: "${SESSION_SECRET:-dev-only-change-me}"`.
- In dev mode, `internal/email/sender.go:52` extracts and logs every link in outbound
  emails — including `?auth=` one-time login tokens — to stdout. Combined with the known
  fallback secret, a naive `docker compose up` deployment logs plaintext auth tokens and
  signs CSRF cookies with a publicly known key.
- **Fix:** default `DEV_MODE` to `false`, remove the `SESSION_SECRET` fallback so compose
  fails loudly when unset, and strip `auth=` query params from logged links regardless of mode.

#### 2. Duplicate-email risk in the scheduler
- If a reminder or reaction-digest batch sends successfully but `MarkEventFired` then fails
  (`internal/scheduler/scheduler.go:202`), the event remains pending and re-fires on the
  next 60s tick (or startup catch-up) — every recipient gets the email again.
- Neither `SendReminderForIssue` (`internal/server/actions.go:79`) nor `SendReactionDigests`
  (`internal/server/actions.go:485`) checks `email_log` for a prior successful send.
- **Fix:** check for an existing `status='sent'` row in `email_log` per recipient before
  transmitting, or make the action + `MarkEventFired` atomic.

#### 3. `ToggleReaction` race condition
- `internal/store/reaction.go:53–79`: the delete-then-maybe-insert runs as two bare
  `ExecContext` calls with no transaction. Two concurrent toggles for the same
  `(userID, responseID, emoji)` can both see 0 rows deleted and both reach the INSERT;
  the loser errors or returns a wrong `added` value.
- **Fix:** wrap in a `BeginTx`/`Commit` pair, matching the pattern in `AutosaveResponse`.

#### 4. Stored-HTML XSS surface in the frontend
- `static/social.js:89`: `c.body_html` (server-rendered Markdown) is injected straight into
  `innerHTML`. Safe today only because Goldmark strips raw HTML by default — one config
  change (`WithUnsafe()`) away from stored XSS, and the CSP (`'unsafe-inline'` scripts)
  provides no backstop.
- `templates/page/profile.html:93`: avatar preview injects a user-typed URL via `innerHTML`
  (self-XSS via `x" onerror="..."`). Use `img.src = avatar` instead.
- `templates/page/albums.html:45,47`: `item.url` is interpolated unescaped into
  `data-lightbox-src` and `<img src>`; escape it or use `setAttribute()`.

### Medium priority

#### 5. Open redirect in dev login
- `internal/server/auth.go:56–61`: `handleDevLogin` passes `?redirect=` directly to
  `http.Redirect` — exploitable anywhere `DEV_MODE=true`.
- **Fix:** require the value to start with `/`, else fall back to `/`.

#### 6. Unbounded JSON request bodies
- `internal/server/server.go:513–518`: `readJSON` decodes `r.Body` with no size cap; any
  authenticated user can POST a multi-GB body to any mutation endpoint.
- **Fix:** wrap with `http.MaxBytesReader` (1–4 MB) in `readJSON` or a middleware.

#### 7. Upload type validated from client-supplied header only
- `internal/server/responses.go:425–431`: `isAllowedImageType` trusts the multipart
  `Content-Type` header; an HTML/SVG payload can be stored under an image extension and
  re-sniffed by the browser when served.
- **Fix:** read the first 512 bytes and cross-check `http.DetectContentType` against the allowlist.

#### 8. Magic-link rate limit enables login lockout
- `internal/server/auth.go:128–140`: the 3-per-hour cap is keyed on `user_id` only. Anyone
  who knows an email can burn the 3 tokens and lock that user out of login for an hour.
- **Fix:** add an IP-keyed rate limit on `POST /api/auth/login` in front of the per-user check.

#### 9. Missing HSTS header
- `internal/server/middleware.go:54–89`: security headers omit `Strict-Transport-Security`.
  May be set at the Traefik layer — verify; if not, add it (guarded by `!DevMode`).

#### 10. Read pool opened with `_txlock=immediate`
- `internal/store/store.go:60–70`: both pools share the same DSN, so read transactions also
  acquire IMMEDIATE (write-level) locks, serializing readers against the writer and
  defeating WAL concurrency.
- **Fix:** separate DSNs — keep `_txlock=immediate` only on the write pool.

#### 11. No indexes in the schema
- `internal/store/migrations/001_initial.sql` has zero `CREATE INDEX` statements. Columns
  used in WHERE/JOIN throughout the store: `response_blocks.response_id`,
  `comments.response_id`, `reactions.response_id`, `questions.issue_id`,
  `auth_tokens.user_id`, `sessions.user_id`, `scheduler_events.fired_at`,
  `responses.question_id`.
- **Fix:** add a migration with explicit indexes for each.

#### 12. Deployment/runtime hardening
- `Dockerfile:2` pins `golang:1.25-alpine` — verify against currently released Go tags.
- No `stop_grace_period` in `docker-compose.yml`; the 10s Docker default exactly matches the
  HTTP drain timeout, leaving zero headroom for `sched.Stop()` — an in-flight email batch
  gets SIGKILLed. Add `stop_grace_period: 30s` and/or cancel the scheduler context before
  the HTTP drain.
- `internal/email/sender.go:71–77`: a new SMTP client per email (no reuse across batches),
  and `mail.WithSSL()` hardcodes implicit TLS — no STARTTLS support without a code change.
  Reuse one client per batch and add an `SMTP_TLS_MODE` env var.

### Low priority / hygiene

- `handleVerifyToken` (`internal/server/auth.go:198–283`) never checks `token.Type`, so
  `email_cta` tokens are accepted at `/auth/verify`. Not a privilege escalation — CTA links
  already mint sessions by design via `handleAuthParam`, and both token types land in the
  same inbox — but the two types have different expiry semantics, so add a type check for hygiene.
- `internal/store`: translate `sql.ErrNoRows` to a typed `ErrNotFound` sentinel (mirroring
  `ErrVersionConflict`) so handlers can return 404 instead of 500.
- `GetActiveIssue` (`internal/store/issue.go:86–100`) has no `ORDER BY` and nothing prevents
  two simultaneous active issues; add ordering plus a partial unique index on active statuses.
- `CreateUser` + `EnsureNotificationPreferences` are two non-atomic calls at call sites
  (`internal/server/admin.go:367,376`, `internal/server/onboarding.go:319,327`); wrap both
  in one store method/transaction.
- Migration runner (`internal/store/store.go:129–204`): check-and-apply isn't atomic across
  processes; use `BEGIN IMMEDIATE` and re-check `schema_migrations` inside the transaction.
- `renderPage` (`internal/server/server.go:474–480`) streams directly to the
  ResponseWriter; a mid-render template error produces truncated HTML + a useless
  `http.Error`. Render into a buffer first (as `renderEmail` already does).
- `time.After` timer leak in `SendBatch` (`internal/email/sender.go:133`); use
  `time.NewTimer` + `defer Stop()`.
- `scheduleDailyCleanup` (`internal/scheduler/scheduler.go:221–229`) logs all errors at
  Debug as "usually UNIQUE — benign"; distinguish real failures and log them at Warn/Error.
- `settings.timezone` is stored and exposed in the UI but never used in scheduling — all
  scheduling math is UTC. Either honor it or remove the field.
- Validate `PORT`/`SMTP_PORT` ranges in `internal/config/config.go`.
- Onboarding timezone (`internal/server/onboarding.go:131–133`): `time.LoadLocation`
  failure silently falls back to UTC; surface a validation error instead.
- Save-status bar and toasts lack `aria-live`/`role="status"` — screen readers never hear
  save state changes (`templates/page/respond.html:65`, `static/editor.js`).
- "Submit all" (`templates/page/respond.html:176–204`) silently skips questions with no
  response ID and still reports "Submitted!"; tell the user what was skipped.

---

## Part 2 — Next feature work

In rough priority order, based on spec gaps with real user impact:

1. **Expand the question bank.** `internal/store/seed/questions.json` has 49 questions vs.
   the spec's 500+. `SelectRandomUnusedQuestions` exhausts the bank after ~10 issues — a
   monthly newsletter breaks within a year of real use.
2. **Wire up the Extend Deadline button.** `handleExtendDeadline` is implemented and routed,
   but the admin dashboard template never renders the button (spec §4.3 mockup shows it).
   Cheap win.
3. **Comment threading UI.** `parent_id` is in the schema and `handleAddComment` accepts it,
   but `social.js` renders a flat list with no Reply button. The backend half is done.
4. **HTTP handler integration tests.** Helper-level unit tests exist, but there is zero
   `httptest` coverage of routes. Start with the highest-consequence paths: auth verify,
   CSRF rejection, autosave 409-conflict, and memento public/private access.
5. **Mobile lightbox swipe** (spec §4.5) — `static/lightbox.js` handles keyboard only.
6. **Service worker cache versioning.** `static/sw.js` hardcodes `pol-static-v1`; deploys
   serve stale JS/CSS until the string is manually bumped. Derive the cache key from a
   build hash or version.
7. **ZIP export.** The settings page promises JSON/ZIP; only JSON exists
   (`/api/admin/export`).

## Test coverage summary

**Covered:** store-level auth token atomicity, reaction toggling, question selection,
migration version parsing; server helpers (`uploadURL` traversal defense, cadence math,
embed providers, upload validation, Markdown rendering). Playwright `tests/crawl.mjs` is a
screenshot/console-error smoke crawler, not an assertion suite.

**Not covered:** all HTTP handlers end-to-end, CSRF middleware enforcement, autosave
version-conflict path, scheduler dispatch, email template rendering, memento access
control, export, onboarding wizard, friend question flow.
