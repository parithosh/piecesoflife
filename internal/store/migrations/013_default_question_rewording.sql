-- Tighten the seeded defaults to three short prompts. Migration 011's seed
-- now ships this final set for fresh installs; this catches databases seeded
-- with the earlier long wording (011 as first shipped, or 012's renames).
-- Copies already landed on issues keep the text they were sent with.
UPDATE default_questions
SET text = 'What good thing happened this month?', sort_order = 0
WHERE text = 'Tell us about one good thing that happened to you this month!';

UPDATE default_questions
SET text = 'What bad thing happened this month?', sort_order = 1
WHERE text = 'Tell us about one bad thing that happened to you this month!';

UPDATE default_questions
SET text = 'Free space for random thoughts', sort_order = 2
WHERE text = 'Free Space for random thoughts.';

-- "On your mind" is no longer part of the default set. The admin can add it
-- back as a custom default from Settings.
DELETE FROM default_questions
WHERE text = 'What''s been on your mind?';
