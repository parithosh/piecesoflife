-- Admins now count toward submission progress by default. CreateIssue sets
-- count_admin_in = 1 explicitly for new issues (SQLite cannot change a
-- column default without a table rebuild); this flips rounds that are still
-- open so the change applies immediately. Published issues keep whatever
-- the admin chose at the time.
UPDATE issues SET count_admin_in = 1 WHERE status IN ('draft', 'collecting');
