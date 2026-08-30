-- 022: freeze an upcoming issue's question set once an admin starts curating it.
--
-- Uncurated drafts keep the existing behavior: member suggestions remain open,
-- and defaults plus bank picks are added when the issue starts. A timestamp
-- means the exact stored set has been prepared and must open unchanged.

ALTER TABLE issues ADD COLUMN questions_curated_at DATETIME;
