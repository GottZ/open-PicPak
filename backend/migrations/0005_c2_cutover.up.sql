-- open-picpak backend - schema v5 (Doc 15 cutover): drop the legacy HOTP/boot_count auth columns.
-- Devices now authenticate ONLY via the per-session secret established by the signed ECDSA re-key
-- (session_secret/epoch/last_counter/bootstrapped + ecdsa_pubkey, added in 0004). The composite
-- boot_count counter — and its bc-lockout — no longer exists. Forward-only; idempotent.
-- NOTE: deploy the session-only ingest (which no longer SELECTs these columns) before/with this.
ALTER TABLE device_auth
  DROP COLUMN IF EXISTS hotp_secret,
  DROP COLUMN IF EXISTS last_boot_count,
  DROP COLUMN IF EXISTS last_rtc_counter;
