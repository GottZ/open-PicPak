-- 0016_templates.down.sql — reverse the template store. Drop the FK column first (drops its index
-- too), then the table. Repo convention: every migration ships a paired .down.sql.
ALTER TABLE faas_functions DROP COLUMN IF EXISTS template_id;
DROP TABLE IF EXISTS templates;
