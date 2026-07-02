-- Auto-create is the intended default rhythm: publishing one issue schedules
-- (and pre-creates) the next. Flip existing installs on; SeedSettings seeds
-- new databases with it on as well.
UPDATE settings SET auto_create_enabled = 1;
