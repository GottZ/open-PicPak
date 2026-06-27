-- open-picpak backend - schema v3 DOWN. Drops the C2 command queue + cursor (Wave 3c).
-- NEVER auto-run; manual rollback only.
DROP TABLE IF EXISTS device_c2_cursor;
DROP TABLE IF EXISTS command_queue;
