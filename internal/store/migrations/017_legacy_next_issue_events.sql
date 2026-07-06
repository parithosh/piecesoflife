-- 017: attach legacy create_next_issue events to an issue of their Loop.
--
-- Before multi-group, create_next_issue events were queued with a NULL
-- issue_id — there was only one group, so no attribution was needed. The
-- scheduler now resolves an event's Loop through its issue, and a NULL
-- reference would need runtime guesswork (which goes wrong the moment the
-- original Loop is archived). All pre-016 data lives in group 1, so pending
-- legacy events can only belong to it: point them at group 1's most recent
-- issue. Events that cannot be attributed (no issues at all — nothing was
-- ever published, so nothing queued them) are dropped rather than guessed.

UPDATE scheduler_events
SET issue_id = (
    SELECT id FROM issues WHERE group_id = 1
    ORDER BY created_at DESC, id DESC LIMIT 1
)
WHERE issue_id IS NULL
  AND event_type = 'create_next_issue'
  AND fired_at IS NULL
  AND EXISTS (SELECT 1 FROM issues WHERE group_id = 1);

DELETE FROM scheduler_events
WHERE issue_id IS NULL
  AND event_type = 'create_next_issue'
  AND fired_at IS NULL;
