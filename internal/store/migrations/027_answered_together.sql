-- 027: hot-seat questions answered together.
-- A member with two overlapping hot-seat questions can answer them in one
-- answer. The joined question points at its primary, whose response is the
-- shared answer; the joined question never has a response of its own. A
-- primary is always a root (answered_with IS NULL). Deleting a primary
-- (only possible while it is unanswered) makes its joined questions
-- separate again.

ALTER TABLE questions ADD COLUMN answered_with INTEGER
    REFERENCES questions(id) ON DELETE SET NULL;
