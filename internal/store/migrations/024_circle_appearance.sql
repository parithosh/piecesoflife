-- 024: per-circle appearance (fabric, colours, circle photo, banner).
--
-- settings.theme holds the circle's published appearance as versioned JSON
-- (NULL = the house look); theme_previous holds the look it replaced, for
-- one-step "Restore previous look". The instance switch is a ceiling that
-- defaults OFF, so an upgraded instance renders exactly as before until the
-- operator opts in.
--
-- accent_color is dropped: it was never authorable (no UI wrote it) and
-- the Pallu stylesheet never read the --accent it fed.
ALTER TABLE settings ADD COLUMN theme TEXT;
ALTER TABLE settings ADD COLUMN theme_previous TEXT;
ALTER TABLE settings DROP COLUMN accent_color;

ALTER TABLE instance_settings ADD COLUMN allow_circle_appearance BOOLEAN NOT NULL DEFAULT 0;
