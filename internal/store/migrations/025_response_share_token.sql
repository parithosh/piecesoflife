-- 025: unguessable share tokens for public mementos (/m/{token}).
--
-- Public memento URLs previously used the sequential responses.id primary
-- key, allowing unauthenticated scrapers to enumerate every published answer
-- in any circle with AllowPublicMementos enabled. Each response now carries
-- a 128-bit random hex share_token used in /m/{token}.
ALTER TABLE responses ADD COLUMN share_token TEXT NOT NULL DEFAULT '';

UPDATE responses
SET share_token = lower(hex(randomblob(16)))
WHERE share_token = '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_responses_share_token
ON responses(share_token)
WHERE share_token != '';
