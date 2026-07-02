-- Per-issue "count me in this round" flag for admins.
--
-- Progress historically counted every active user (admins included) in the
-- expected-responders denominator, which read oddly (e.g. "0 / 2" when the
-- only non-admin hadn't answered). Admins are now excluded from progress by
-- default; this flag lets an admin opt back into an individual round.
ALTER TABLE issues ADD COLUMN count_admin_in BOOLEAN NOT NULL DEFAULT 0;
