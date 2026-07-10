-- open-picpak backend - schema DOWN for 0017. Reverts the octet-cap + enqueue-dedup guard (A30 Wave 3).
-- NEVER auto-run; manual rollback only. Idempotent (DROP IF EXISTS + CHECK re-add).
DROP INDEX IF EXISTS command_queue_pending_dedup;
ALTER TABLE command_queue DROP COLUMN IF EXISTS applied;
ALTER TABLE command_queue DROP COLUMN IF EXISTS idempotency_key;
ALTER TABLE command_queue DROP CONSTRAINT IF EXISTS command_script_len;
ALTER TABLE command_queue ADD CONSTRAINT command_script_len CHECK (char_length(script) <= 16384);
