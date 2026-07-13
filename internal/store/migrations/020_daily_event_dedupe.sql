-- 020: actually dedupe the daily scheduler events.
--
-- UNIQUE(issue_id, event_type, scheduled_at) never held for the daily
-- events (token/session cleanup, comment digest): their issue_id is NULL,
-- and SQLite treats NULLs as distinct inside UNIQUE constraints, so every
-- 60-second scheduler tick quietly inserted a fresh duplicate for the next
-- midnight — and at midnight the whole pile fired back-to-back. Harmless
-- for the idempotent cleanups, but the comment digest must not get 1,440
-- chances a night to retry a failing recipient's email.
--
-- Collapse the accumulated duplicates, then add the partial unique index
-- the code always assumed, so EnsureDailyEvent's INSERT OR IGNORE dedupes
-- for real.

DELETE FROM scheduler_events
WHERE issue_id IS NULL
  AND id NOT IN (
    SELECT MIN(id) FROM scheduler_events
    WHERE issue_id IS NULL
    GROUP BY event_type, scheduled_at
  );

CREATE UNIQUE INDEX IF NOT EXISTS ux_scheduler_events_daily
    ON scheduler_events(event_type, scheduled_at)
    WHERE issue_id IS NULL;

-- 018 created these redundantly — the UNIQUE constraints on
-- rambles(user_id, day) and diary_days(section_id, day) already provide
-- identical indexes.
DROP INDEX IF EXISTS idx_rambles_user_day;
DROP INDEX IF EXISTS idx_diary_days_section_day;
