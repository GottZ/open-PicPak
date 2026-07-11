-- 0019_template_preview.down.sql — reverse the durable preview cache (W-A33.5a).
-- Repo convention: every migration ships a paired .down.sql.
DROP TABLE IF EXISTS template_preview;
