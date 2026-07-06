# Upgrading an existing instance

PiecesOfLife migrates its own database. Schema migrations ship inside the
binary and apply automatically the first time a new release boots — there is
no separate migration command to run. This document is the operator's side
of that story: what happens, how to prepare, how to verify, and how to roll
back.

## What happens on upgrade

1. On startup the app compares the embedded migration list against the
   `schema_migrations` table.
2. **If migrations are pending on an existing database, the app first writes
   a consistent snapshot next to the live file** —
   `piecesoflife.db.backup-before-<version>-<timestamp>` (made with SQLite's
   `VACUUM INTO`; a single file, no `-wal`/`-shm` siblings needed). If the
   snapshot cannot be written (e.g. disk full), the app refuses to migrate
   and exits with a clear error rather than upgrading without a rollback
   path.
3. Each pending migration runs in its own transaction. Migrations that
   rebuild referenced tables additionally verify `foreign_key_check` before
   committing.
4. The log records every step: `Database backed up before migration` (with
   the backup path) followed by one `Applied migration` line per version.

Fresh installs skip the snapshot — there is nothing to protect yet.

## Standard upgrade (docker compose)

```sh
docker compose pull        # or: git pull && docker compose build
docker compose up -d
docker compose logs -f app # watch for "Applied migration" + healthy
curl -fsS localhost:8080/health
```

Old snapshots accumulate in the data directory (one per upgrade that had
pending migrations). Delete them whenever you're satisfied with a release:

```sh
docker run --rm -v <project>_app-data:/data alpine \
  sh -c 'ls -lh /data/db/*.backup-before-*'
```

## Rolling back

A migration is one-way — downgrading the binary against an upgraded database
is not supported. To roll back:

1. Stop the app: `docker compose down`.
2. Restore the snapshot **and remove the WAL siblings of the live file**
   (they belong to the new schema):

   ```sh
   docker run --rm -v <project>_app-data:/data alpine sh -c '
     cd /data/db &&
     rm -f piecesoflife.db piecesoflife.db-wal piecesoflife.db-shm &&
     cp piecesoflife.db.backup-before-016-<timestamp> piecesoflife.db &&
     chown 100:101 piecesoflife.db'
   ```

   (`100:101` is the app container's user; a root-owned file yields
   *attempt to write a readonly database*.)
3. Start the **previous** image version: `docker compose up -d` with the old
   tag. Anything written between the upgrade and the rollback is lost — the
   snapshot is from the moment before migration.

## Rehearsing an upgrade first (recommended for majors)

Snapshot the live database and boot the new image against a copy:

```sh
docker run --rm -v <project>_app-data:/data -v "$PWD":/backup alpine \
  cp /data/db/piecesoflife.db /backup/rehearsal.db
# then point a scratch compose file / container at rehearsal.db and watch
# the logs. Rehearse against real data — fresh databases have empty
# referencing tables and won't catch rebuild edge cases.
```

## Version notes

### v016 — multi-group ("many Loops, one instance")

The largest migration to date; see `docs/multi-group.md` for the design.
What it does to an existing single-group database:

- Everything you have becomes **group 1**: your loop's settings, issues,
  question bank, and default questions are re-keyed under it. No content
  changes.
- **Roles move**: `users.role` disappears; per-Loop roles live on the new
  `memberships` table. Existing admins become admins *of your Loop*. Only
  the `ADMIN_EMAIL` account becomes the **instance admin** (can create and
  archive Loops from the new `/instance` console) — co-admins keep exactly
  their Loop, nothing more.
- A new `instance_settings` row is created, inheriting your loop name as
  the instance name and your public-memento setting as the instance policy.
  Both are editable at `/instance`.
- Sessions survive; members notice nothing until a second Loop exists.

After upgrading, verify:

- the log shows `Applied migration … 016_groups.sql` and the backup line;
- `/health` returns ok and members can log in and see the archive;
- as the operator, `/instance` lists your Loop with the right member count.
