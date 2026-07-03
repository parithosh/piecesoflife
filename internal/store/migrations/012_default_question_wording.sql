-- Rename the seeded default questions to fuller, friendlier prompts.
-- Migration 011's seed now ships the new wording for fresh installs; this
-- catches databases that already ran 011 with the short names (no-op
-- otherwise). Copies already landed on issues keep the text they were sent
-- with — the admin can reword those per issue from the dashboard.
UPDATE default_questions
SET text = 'Tell us about one good thing that happened to you this month!'
WHERE text = 'One good thing';

UPDATE default_questions
SET text = 'Tell us about one bad thing that happened to you this month!'
WHERE text = 'One bad thing';

UPDATE default_questions
SET text = 'What''s been on your mind?'
WHERE text = 'On your mind';

UPDATE default_questions
SET text = 'Free Space for random thoughts.'
WHERE text = 'Free space';
