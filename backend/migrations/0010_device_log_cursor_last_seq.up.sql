-- 0010: device_log_cursor.last_seq — the persistent per-device monotone log sequence (A21 / D21.9).
--
-- The reassembly path stamps every viewer `logs` row with a server-assigned `seq` (monotone per
-- device). That seq must OUTLIVE the 90d `logs` retention because it is the keyset tiebreak (D21.11):
-- deriving it from MAX(logs.seq) would reset after retention drops the oldest chunk AND would race two
-- concurrent same-device pushes. So it lives on the single cursor row, advanced under the same per-serial
-- FOR UPDATE lock that serializes the offset-ack (D21.9). Idempotent + re-runnable against the final
-- schema; appended to the compose `migrate -f` list (single source of truth).
ALTER TABLE device_log_cursor ADD COLUMN IF NOT EXISTS last_seq BIGINT NOT NULL DEFAULT 0;
