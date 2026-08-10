# Incident 2026-08-10: scheduler wedged on DB write, login emails dead for 7h

Handoff doc for fixing the root cause. Written after a log-based investigation on the
production host; no code changes have been made yet. The evidence below is from the live
container logs, the fix work happens in this repo.

## TL;DR

At 00:00:58 UTC on 2026-08-10 the in-app scheduler completed `token_cleanup` and then
**never completed `session_cleanup`**. From that moment every DB **write** in the app hung
forever (reads kept working). A user login at 07:17 hung for 73s at "recording login
attempt" and never reached the email-send step. Two manual `docker restart`s at ~07:18/07:19
cleared it; the fresh process fired the pending midnight events instantly and everything
worked again.

The strong hypothesis (to be confirmed in code): **write-connection starvation on the
single-connection write pool**, i.e. a Go-level `database/sql` deadlock — NOT a SQLite lock
issue. `busy_timeout=5000` is already set and would have surfaced as an `SQLITE_BUSY` error
after 5s, not an infinite silent hang. Something acquired the one write connection (or a
`Tx`/`Rows` pinning it) and then either never released it, or code requested a second write
connection while holding the first, which blocks forever when `ctx` has no deadline.

## Deployment context

- Runs as `parithoshj/piecesoflife:1.4.3` on host `remoteBoi` (NixOS, docker-compose stack
  `piecesoflife-compose.service`, two instances: `piecesoflife` = main, `piecesoflife-triplem`).
  Only the **main** instance wedged; triplem sailed through the same midnight fine — so this
  is a race, not deterministic.
- Env: `DATABASE_PATH=/data/db/piecesoflife.db`, `LOG_FORMAT=json`, `PORT=8080`,
  emails via JMAP/Fastmail. Process had been up since 2026-08-05.
- The Docker healthcheck (`/health`) stayed **green the whole time** because it only
  exercises reads. Nothing alerted.

## Timeline (all UTC, from `docker logs` of the main container)

Normal nights for comparison — cleanups complete back-to-back within ~1ms:

```
2026-08-08T00:00:58.758 "Token cleanup complete"   event_id=125170
2026-08-08T00:00:58.760 "Session cleanup complete" event_id=125171
2026-08-09T00:00:58.754 "Token cleanup complete"   event_id=129490
2026-08-09T00:00:58.754 "Session cleanup complete" event_id=129491
```

Incident night:

```
2026-08-10T00:00:58.778 "Token cleanup complete"   event_id=133810 event_type=token_cleanup deleted=0
                        <-- NO "Session cleanup complete" ever follows. Scheduler wedged here. -->
... 7 hours of perfectly healthy GET /health, GET /login, static assets (all reads, all 200) ...
2026-08-10T07:17:26     POST /api/auth/login starts (derived: completion time minus duration_ms)
2026-08-10T07:18:30.544 "Shutdown signal received"            <-- user's docker restart #1 (SIGTERM)
2026-08-10T07:18:40.546 ERROR "Magic link request failed" error="recording login attempt: recording login attempt: sql: database is closed"
2026-08-10T07:18:40.548 POST /api/auth/login status=500 duration_ms=73720
2026-08-10T07:18:40     ERROR "Fatal error" error="server shutdown: context deadline exceeded"  <-- 10s graceful-shutdown deadline blown by the stuck request
2026-08-10T07:18:42.808 "Starting PiecesOfLife"               <-- fresh process
2026-08-10T07:18:43.107 "Firing late event" event_type=session_cleanup scheduled_at=2026-08-10T00:00:00Z delay=26323s (7h18m!)
2026-08-10T07:18:43.107 "Session cleanup complete" deleted=0  <-- instant. The job is trivial; the old process was wedged.
2026-08-10T07:18:43.107 "Firing late event" event_type=comment_digest scheduled_at=2026-08-10T00:00:00Z
2026-08-10T07:19:24.685 POST /api/auth/login status=200 duration_ms=3
2026-08-10T07:19:31.544 "JMAP session initialized"
2026-08-10T07:19:32.182 "Email sent" subject="Your login link"
2026-08-10T07:19:39.255 "Shutdown signal received"            <-- accidental restart #2, harmless
2026-08-10T07:19:47.692 GET /auth/verify 303                  <-- login link worked (token persisted in DB)
```

## External causes ruled out (checked on the host)

- `docker-prune` + `nix-gc` weekly timers fired 00:00:09 but finished by 00:00:24
  (nix-gc: 15s wall). The wedge started at 00:00:58 — after both were done.
- `fstrim` ran 00:13, after the wedge. restic backup ran 03:00 as always.
- No kernel warnings, OOM, or IO errors in the journal around midnight.
- DB confirmed healthy WAL mode (`journal_mode=WAL`, `-wal`/`-shm` present, header
  write-version 2). No stale locks on disk after restart.

## What we know about the code (starting points)

- `internal/store/store.go` — split pools:
  - `writeDB` with `SetMaxOpenConns(1)` (line ~70)
  - `readDB` with `SetMaxOpenConns(4)`
  - PRAGMAs: `journal_mode=WAL`, `busy_timeout=5000`
  - Driver: `modernc.org/sqlite` or mattn — check the import; `sql.Open("sqlite", ...)`
    suggests modernc.
- `internal/scheduler/scheduler.go` — the event loop. The wedge is in the window
  **after logging "Token cleanup complete" and before logging "Session cleanup complete"**,
  i.e. in the mark-done / claim-next / execute path between two consecutive events.
- The login handler's first write is "recording login attempt" (double-wrapped error string
  `recording login attempt: recording login attempt: ...` — also worth fixing the
  duplicate wrap; grep for that message, likely in `internal/auth/` or `internal/server/auth.go`).
  It hung for 73s with no 5s busy-timeout error → it was **waiting for the write pool
  connection**, not for a SQLite lock. `sql.ErrConnDone`-style pool waits with
  `context.Background()` block indefinitely.

## Diagnosis to confirm

With `writeDB.SetMaxOpenConns(1)`, any of these produces exactly the observed behavior
(scheduler + all writers hang forever; readers fine; only resolved by process death):

1. **Nested acquisition**: code begins a `Tx` (or `Conn`) on `writeDB`, then inside it calls
   a store method that uses `writeDB.Exec/Query` directly → that call waits for a free
   connection that the enclosing `Tx` holds → deadlock. Classic `database/sql` +
   `MaxOpenConns(1)` footgun.
2. **Leaked `Rows`**: a `writeDB.Query(...)` whose `rows.Close()` is skipped on some path
   (early return, error branch) pins the single connection forever.
3. **Leaked `Tx`**: a code path that returns without `Commit`/`Rollback` (e.g. panic
   swallowed, or an error branch missing rollback).

Audit the scheduler's event claim/complete cycle first (that's where it stuck), then every
use of `writeDB` for patterns 1–3. `go vet` + `sqlclosecheck`/`rowserrcheck` linters help.
Note the race is rare (first occurrence in ~5 days of uptime, nightly runs fine before), so
expect a timing-dependent branch — likely one that only triggers when a concurrent write
(request-path) overlaps the midnight batch, or an error branch that skips cleanup.

## Required fixes (defense in depth, not just the root cause)

1. **Find and fix the actual deadlock** (above). A regression test that runs the scheduler
   tick concurrently with request-path writes against a `MaxOpenConns(1)` store, under
   `-race` and with a test timeout, should reproduce/guard it.
2. **Per-job timeout in the scheduler**: wrap each event execution in
   `context.WithTimeout` (say 1–2 min), pass it through to all store calls
   (`ExecContext`/`QueryContext` — audit that store methods actually take and propagate
   `ctx`; pool acquisition respects context deadlines, so this alone converts the
   infinite wedge into a logged ERROR + skip/retry). One stuck job must never stop the
   loop: log at ERROR, mark the event failed, continue. The "Firing late event" recovery
   logic already exists and works — lean on it.
3. **Deadline on request-path writes**: HTTP handlers should use the request context (with
   a server-side write timeout) for store calls, so a user request fails in seconds with a
   500 instead of hanging 73s and blowing the graceful-shutdown budget.
4. **Liveness in `/health`**: current healthcheck only proves reads work. Add a scheduler
   heartbeat (e.g. store `last_tick` in memory/atomic; `/health` returns 5xx if the
   scheduler hasn't ticked in > N minutes, and optionally do a trivial write like a
   `PRAGMA user_version` bump or heartbeat-table upsert with a short-timeout ctx). That
   flips the Docker healthcheck to unhealthy so the wedge is visible/actionable — during
   this incident the container sat "healthy" for 7 hours.
5. **Cosmetic**: de-duplicate the `recording login attempt:` double error wrap.

### Explicitly NOT the fix

`busy_timeout` tuning — it is already 5000ms and this incident never involved a SQLite-level
lock wait (that errors after 5s; we observed an infinite hang, which lives in Go's
connection-pool layer). Don't raise it; it would change nothing here.

## Verification / deploy notes

- Verify with the nightly signature: `Token cleanup complete` + `Session cleanup complete`
  must appear back-to-back; add a completion log for `comment_digest` too (it currently
  logs nothing on quiet nights, which made this harder to debug).
- Tests: table-driven, `testify`, run with `-race` (repo standard).
- Deploy path: bump image tag (currently `parithoshj/piecesoflife:1.4.3` — updates are
  deliberate tag bumps, no pull-on-restart), push to Docker Hub, then bump the tag in the
  homelab repo `services/compose/piecesoflife/compose.yaml` (both instances) and let comin
  roll it out on remoteBoi.
- Repo is currently on `feature/newsletter-pdf-importer`; branch the fix from `main`.
