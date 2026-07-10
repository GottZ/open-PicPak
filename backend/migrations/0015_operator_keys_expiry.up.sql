-- 0015_operator_keys_expiry.up.sql — operator_keys expiry (A28 W7, design §5 B8). Before the SSO
-- removal (W8) makes admin public, a permanently-valid RCE-capable operator_key is a standing
-- leak amplifier: operator.Authenticate now rejects a key past expires_at, and rotate-operator sets
-- a grace expiry on the old key so a soft rotation (mint replacement, retire on a window) is possible
-- without a hard disable. NULL = no expiry (existing keys keep their behaviour; the migration does not
-- expire the incumbent admin). Idempotent (IF NOT EXISTS) + independent + re-runnable (parity 0014).
ALTER TABLE operator_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
