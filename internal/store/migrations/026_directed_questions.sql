-- 026: directed questions (@mentions in question suggestions).
-- A question with target_user_id set is answered only by that member during
-- collection, and aggregated into "The hot seat" spread when published.

ALTER TABLE questions ADD COLUMN target_user_id INTEGER REFERENCES users(id);

CREATE INDEX IF NOT EXISTS idx_questions_issue_target
    ON questions(issue_id, target_user_id);
