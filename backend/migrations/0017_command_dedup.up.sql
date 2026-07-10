-- open-picpak backend - command_queue octet-cap fix (K3) + enqueue-dedup guard (K4). A30 Wave 3.
--
-- K3 (poison-pill, masterplan): the firmware fetches a C2 script into malloc(C2_RESP_MAX) with
-- C2_RESP_MAX = 8192 (cmd.c:140) and caps the body at C2_RESP_MAX-1 = 8191 bytes to keep room for the
-- NUL terminator (cmd.c:363 buf[len]='\0'); a longer response overflows net.c's fetch buffer -> the
-- device fails WITHOUT an ack (no truncated script) -> a wedged C2 queue, fleet-wide. The old CHECK also
-- counted char_length (CODEPOINTS): a multibyte script under 16384 codepoints could still exceed the byte
-- cap. Fail-closed: octet_length(script) <= 8191 (the exact FW-accepted maximum, byte-measured).
--
-- K4 (fleet double-effect, masterplan / E-A30-5): the append-only queue double-executes a double-clicked
-- "apply to fleet" -> a doubled net_clear/reboot across the WHOLE fleet. idempotency_key + a partial
-- UNIQUE index over not-yet-applied keyed rows dedups an identical still-pending command to a single row;
-- once the target(s) applied it (applied=true, set on ack, commandstore.MarkApplied) an identical command
-- may enqueue again. A NULL key = no dedup (legacy/keyless enqueues stay append-only).
--
-- golang-migrate convention (.up.sql / .down.sql); forward-only; idempotent (re-runnable).

-- octet-based length CHECK. DROP-then-ADD is the idempotent path (Postgres has no ADD CONSTRAINT IF NOT
-- EXISTS for CHECK); the ADD validates existing rows, so any legacy >8191-byte row must be pruned first.
ALTER TABLE command_queue DROP CONSTRAINT IF EXISTS command_script_len;
ALTER TABLE command_queue ADD CONSTRAINT command_script_len CHECK (octet_length(script) <= 8191);

ALTER TABLE command_queue ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE command_queue ADD COLUMN IF NOT EXISTS applied BOOLEAN NOT NULL DEFAULT false;

-- Partial UNIQUE index = the enqueue-dedup guard AND the ON CONFLICT arbiter. Covers only still-pending
-- keyed rows: an acked row (applied=true) leaves the index -> an identical command re-enters (single
-- serial). A '*' row is applied per device and never flipped by one device's ack -> the fleet dedup
-- persists while any device lags (the intended fleet double-effect guard).
CREATE UNIQUE INDEX IF NOT EXISTS command_queue_pending_dedup
    ON command_queue (serial, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND NOT applied;
