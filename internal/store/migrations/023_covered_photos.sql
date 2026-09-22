-- Viewer-controlled photo covers. This is presentation metadata, not an
-- authorization boundary: members may reveal covered photos at any time.
ALTER TABLE response_blocks ADD COLUMN is_covered BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE response_blocks ADD COLUMN cover_note TEXT;

ALTER TABLE dump_items ADD COLUMN is_covered BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE dump_items ADD COLUMN cover_note TEXT;

ALTER TABLE ramble_blocks ADD COLUMN is_covered BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE ramble_blocks ADD COLUMN cover_note TEXT;

ALTER TABLE diary_blocks ADD COLUMN is_covered BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE diary_blocks ADD COLUMN cover_note TEXT;
