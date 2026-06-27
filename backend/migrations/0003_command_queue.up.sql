-- open-picpak backend - schema v3 (Wave 3c): C2 command queue + per-device cursor.
-- The device polls a C2 endpoint and executes the returned Berry script (Doc 13 Wave 3).
--
-- Append-only command feed with a GLOBAL monotonic seq; each device tracks its applied cursor
-- (mirrors device_log_cursor). The cursor is the ONLY per-device state -> a fleet ('*') command is
-- served to every device independently and acked per device (no per-row state, no per-device fan-out).
-- Policy as data: the command scripts are data (Berry), not code constants.
-- golang-migrate convention (.up.sql / .down.sql); forward-only; idempotent (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS command_queue (
    seq         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,  -- GLOBAL monotonic (NOT per-device)
    serial      TEXT NOT NULL,                 -- target device SN, or '*' = whole fleet
    script      TEXT NOT NULL,                 -- the Berry command (data, not a code constant)
    note        TEXT,                          -- operator label (free text)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- bound the script size (mirrors the FW C2_SCRIPT_MAX cap, well under the WiFi+VM RAM spike)
    CONSTRAINT command_script_len CHECK (char_length(script) <= 16384)
);
CREATE INDEX IF NOT EXISTS command_queue_serial_seq ON command_queue (serial, seq);

CREATE TABLE IF NOT EXISTS device_c2_cursor (
    serial       TEXT PRIMARY KEY REFERENCES devices(serial) ON DELETE CASCADE,
    applied_seq  BIGINT NOT NULL DEFAULT 0,     -- highest GLOBAL seq the device has applied (its ack)
    last_poll_at TIMESTAMPTZ,                   -- last C2 poll seen (liveness)
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The cursor only moves forward (the serve handler advances it with GREATEST) -> a replayed low ack
-- can never roll it back. A device must be registered (devices row, via ingest/provisioning) before it
-- can poll C2 -> the FK rejects an unknown serial.
