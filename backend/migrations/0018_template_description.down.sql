-- 0018_template_description.down.sql — reverse the multilingual description column (A34.4).
-- Repo convention: every migration ships a paired .down.sql.
ALTER TABLE templates DROP COLUMN IF EXISTS description;
