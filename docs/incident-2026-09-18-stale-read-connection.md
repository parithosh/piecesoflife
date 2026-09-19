# Incident 2026-09-18: stale read connection re-opened a round 19 times, 54 emails

## TL;DR

From 06:36:30 to 06:53:30 UTC the TripleM scheduler fired the same four
`scheduler_events` rows every tick — three daily cleanups from **September 5**
and the `create_next_issue` for issue 3, which had already opened on
**September 15**. Each tick re-opened issue 3, inserted another set of default
and bank questions, and emailed all three members "September 2026 is open!".
The operator stopped the container at 06:54.

Root cause: a **pooled read connection stuck inside an open read transaction
since 2026-09-04 08:58**. `modernc.org/sqlite v1.37.1` leaks the prepared
statement when a query's context is cancelled in the window after the
statement has produced its first row (`stmt.query`: the `driver.Rows` is
dropped without `Close()`, so `sqlite3_finalize` never runs). `database/sql`
sees an ordinary context error, not `ErrBadConn`, and returns the connection to
the free pool. Every later query on that connection reads the frozen snapshot;
the WAL checkpointer can never advance past its read mark. Fixed upstream in
v1.49.1 (`stmt.go`: `if r != nil { r.Close() }` in the cancelled branch).

Writes were never affected — the write pool is a separate connection.

## Evidence

Container `piecesoflife-triplem`, image 1.6.0, up since 2026-08-30 20:49 UTC.

| Observation | Meaning |
|---|---|
| `email_log` 40–93: 54 `open` rows 06:36–06:53; issue 3 `collecting`; issue 3 has 57 `default` + 19 `bank` questions | Writes landed in the live file — 19 × (3 defaults + 1 bank + 3 emails). |
| event 59822 `create_next_issue` (due 09‑15 09:00 CEST) `fired_at` = 09‑18 06:53:30; `email_log` 37–39 at 09‑15 09:00:31 | Issue 3 genuinely opened on time on the 15th. The loop overwrote `fired_at` 19×. |
| events 129828–130 (dailies for 09‑05) `fired_at` = 09‑18 06:53:30 | The reader that served the loop had a snapshot from 09‑04 (rows created 09‑04 00:00:30, never seen fired). |
| dailies 09‑08 → 09‑14 all `fired_at` 09‑14 08:12:30; dailies 09‑06 fired 09‑06 20:56:30 | Other frozen readers (snapshots ≈ 09‑05 and ≈ 09‑07) served the scheduler for days; nothing looked due. |
| `piecesoflife.db` mtime **09‑04 08:58:30**; `-wal` **565 MB**; "WAL checkpoint incomplete, retrying next pass" every 6 h | No frame checkpointed since 09‑04 08:58 — a read transaction was held continuously from then on. |
| `PRAGMA integrity_check` ok; same inode since 08‑05; single container start; `.state` generation 1 | No file swap, no corruption, no restart, no rollback. |
| `/health` 200 throughout | Scheduler heartbeat was fine; the loop *was* the scheduler working. |

Why the writer's own guards did not stop it: `shouldSkipEvent` and
`CreateNextIssue`'s `HasCollectingIssue`/`GetNextDraftIssue` all revalidate
through the same poisoned read pool, and `MarkEventFired` was a bare `UPDATE`
with no rows‑affected check. Every safety net read the same lie.

## Fix (this repo)

1. `modernc.org/sqlite` v1.37.1 → v1.59.0 (leak fixed from v1.49.1).
2. `store.New`: `readDB.SetConnMaxLifetime(10m)` — a poisoned read
   connection can no longer outlive ten minutes, whatever leaks it.
3. `store.MarkEventFired` fails when 0 rows were updated.
4. `store.IsEventPending` reads through the **write** connection (never inside
   a stale snapshot); `scheduler.fireEvent` consults it before acting and
   refuses events the write side already fired. Regression test:
   `TestEventAlreadyFiredOnWriteSideIsNotReplayed`.

## Cleanup on remoteBoi (TripleM)

The database is consistent; only issue 3's duplicate questions and the bank
`used` flags need reverting. Order matters: backup, fix, checkpoint, chown,
deploy.

The TripleM container must stay **stopped** until step 4.

### 1. Backup

```sh
docker run --rm -v /opt/piecesoflife/triplem/db:/db alpine:3.21 sh -c \
  'apk add -q --no-cache sqlite && sqlite3 /db/piecesoflife.db \
   "VACUUM INTO '\''/db/piecesoflife.db.backup-before-loop-cleanup-20260919'\''"'
```

`VACUUM INTO` writes a compact single-file snapshot (WAL folded in) next to
the live file. Nothing else is touched.

### 2. Fix

```sh
docker run --rm -i -v /opt/piecesoflife/triplem/db:/db alpine:3.21 sh -c \
  'apk add -q --no-cache sqlite && sqlite3 -header -column /db/piecesoflife.db' <<'SQL'
.bail on
PRAGMA foreign_keys = ON;

-- Guard: refuse to run against a database that is not in the incident's state.
-- abs(INT64_MIN) raises "integer overflow"; with .bail on that aborts the
-- script, and the open transaction below rolls back on exit.
SELECT CASE WHEN (SELECT status FROM issues WHERE id = 3) <> 'collecting'
            THEN abs(-9223372036854775808) END AS guard_issue_3_collecting;

BEGIN IMMEDIATE;

-- Everything the loop inserted on issue 3: created during the incident and
-- never answered. The four original questions from the 15th are untouched.
CREATE TEMP TABLE dup AS
  SELECT q.id, q.text, q.source
  FROM questions q
  WHERE q.issue_id = 3
    AND q.created_at >= '2026-09-18 06:36:00'
    AND NOT EXISTS (SELECT 1 FROM responses r WHERE r.question_id = q.id);

SELECT source, count(*) AS n FROM dup GROUP BY source;   -- expect bank 18, default 54

-- Guard: exactly 72 rows, or abort.
SELECT CASE WHEN (SELECT count(*) FROM dup) <> 72
            THEN abs(-9223372036854775808) END AS guard_dup_count;

-- Return the loop's 18 bank picks to the pool. A text the kept set still
-- uses (it can't, but cheap to be sure) stays used.
UPDATE question_bank SET used = 0
 WHERE group_id = 1
   AND text IN (SELECT text FROM dup WHERE source = 'bank')
   AND text NOT IN (SELECT text FROM questions q
                    WHERE q.issue_id = 3 AND q.id NOT IN (SELECT id FROM dup));
SELECT changes() AS bank_questions_released;             -- expect 18

DELETE FROM questions WHERE id IN (SELECT id FROM dup);
SELECT changes() AS questions_deleted;                   -- expect 72

-- What issue 3 looks like now: 3 default + 1 bank, sort_order 0..3.
SELECT id, source, sort_order, created_at FROM questions
 WHERE issue_id = 3 ORDER BY sort_order, id;

COMMIT;

-- Fold the 565 MB WAL into the main file now that no stale reader pins it.
PRAGMA wal_checkpoint(TRUNCATE);                          -- expect 0|N|N
SQL
```

If either guard trips the script stops before `COMMIT`; nothing is changed.

Run on 2026-09-19 13:45 UTC: guards passed, 72 deleted, **16** bank rows
released (not 18 — the stale reader never saw `used` flip, so it offered
the same bank row twice; 18 rows, 16 distinct texts), checkpoint `0|0|0`
(post-truncation counts; the 565 MB `-wal` had already folded into the
main file when the backup connection closed).

Not touched on purpose: `fired_at` on 59822/129828–130 is cosmetically wrong
(last tick's timestamp) but harmless; the 54 `email_log` rows are history;
the 09‑19 dailies (190312–314) are pending and will fire, late, at boot —
they are idempotent. Issue 3's reminder/auto_close events 178968–971 are
intact and correct.

### 3. Ownership

```sh
sudo chown -R 100:101 /opt/piecesoflife/triplem/db
ls -la /opt/piecesoflife/triplem/db
```

The helper container runs as root; the app runs as uid 100. The `-wal` /
`-shm` files are recreated by whoever opens the database last. Expect the
`-wal` to be gone or tiny after the checkpoint.

### 4. Deploy the fixed image

Tag and push from this repo (`v1.6.1`); GitHub Actions publishes
`parithoshj/piecesoflife:1.6.1`. Then in `homelab`
`services/compose/piecesoflife/compose.yaml`, bump **both** services'
`image:` to the new tag and digest:

```sh
docker pull parithoshj/piecesoflife:1.6.1
docker inspect --format '{{index .RepoDigests 0}}' parithoshj/piecesoflife:1.6.1
```

and roll it out the usual way. Both instances need it — main runs the same
driver and has the same exposure (its 99 MB WAL on 08‑30 was the milder form
of the same symptom).

### 5. Verify

```sh
docker logs -f piecesoflife-triplem | grep -v '"msg":"Request"'
```

Expect: `Scheduler starting`, three `Firing late event` lines for the 09‑19
dailies followed by their `… complete` lines, no `Issue opened for collecting`,
no `Email sent`, no ERROR. Then:

```sh
ls -la /opt/piecesoflife/triplem/db/   # -wal should stay small from now on
```

For main, before deploying, check whether it is also pinned:

```sh
ls -la /opt/piecesoflife/main/db/
docker logs --since 2026-09-01 piecesoflife 2>&1 | grep -c 'checkpoint incomplete'
```

A large `-wal` with a stale `.db` mtime is the same condition; the new image
clears it on restart (all connections close, WAL folds on next open).
