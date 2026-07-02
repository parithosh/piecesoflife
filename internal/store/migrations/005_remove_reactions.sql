-- Local-only cleanup: reactions were removed from the product.
DELETE FROM scheduler_events WHERE event_type = 'reaction_digest';
DELETE FROM email_log WHERE type = 'reaction_digest';
DROP TABLE IF EXISTS reactions;
