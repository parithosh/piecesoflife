-- 003: indexes + scheduler email idempotency
-- Adds the indexes used by common list/join paths and records which
-- scheduler event produced an email log row so retrying an unfired event does
-- not resend mail that already succeeded.

ALTER TABLE email_log ADD COLUMN scheduler_event_id INTEGER REFERENCES scheduler_events(id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_email_log_scheduler_event_user_type
    ON email_log(scheduler_event_id, user_id, type)
    WHERE scheduler_event_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_response_blocks_response_sort
    ON response_blocks(response_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_comments_response_created
    ON comments(response_id, created_at);

CREATE INDEX IF NOT EXISTS idx_comments_parent
    ON comments(parent_id);

CREATE INDEX IF NOT EXISTS idx_questions_issue_sort
    ON questions(issue_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_auth_tokens_user_created
    ON auth_tokens(user_id, created_at);

CREATE INDEX IF NOT EXISTS idx_sessions_user
    ON sessions(user_id);

CREATE INDEX IF NOT EXISTS idx_scheduler_events_pending
    ON scheduler_events(fired_at, scheduled_at);

CREATE INDEX IF NOT EXISTS idx_responses_question
    ON responses(question_id);

CREATE INDEX IF NOT EXISTS idx_email_log_created
    ON email_log(created_at);
