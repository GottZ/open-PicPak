-- 0007 down: drop operator attribution column, then the operator_keys table.
ALTER TABLE command_queue DROP COLUMN IF EXISTS operator_key_id;
DROP TABLE IF EXISTS operator_keys;
