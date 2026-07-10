-- 0015_operator_keys_expiry.down.sql — drop the operator_keys expiry column. IF EXISTS keeps the
-- down re-runnable; no dependent objects reference the column.
ALTER TABLE operator_keys DROP COLUMN IF EXISTS expires_at;
