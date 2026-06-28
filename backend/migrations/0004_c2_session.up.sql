-- open-picpak backend - schema v4 (Doc 15): C2 auth re-architecture.
-- Long-term Ed25519 device identity (the backend holds ONLY the public key) + a per-session HOTP
-- secret established via the signed re-key handshake. This decouples the HOTP counter from boot_count
-- (kills the bc-lockout) and removes the server-held shared secret. The legacy hotp_secret/bc path
-- stays during the bridge window and is dropped in the cutover migration. Idempotent.

-- A bonded device is identified by its Ed25519 public key alone (no shared secret) -> hotp_secret
-- must become nullable. The session secret arrives later via the signed re-key handshake.
ALTER TABLE device_auth ALTER COLUMN hotp_secret DROP NOT NULL;

ALTER TABLE device_auth
  ADD COLUMN IF NOT EXISTS ed25519_pubkey       BYTEA,   -- 32 B; NULL until bonded over USB
  ADD COLUMN IF NOT EXISTS session_secret       BYTEA,   -- 20 B; NULL until first re-key
  ADD COLUMN IF NOT EXISTS session_epoch        BIGINT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS session_last_counter BIGINT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS session_bootstrapped BOOLEAN NOT NULL DEFAULT false;

DO $$ BEGIN
  ALTER TABLE device_auth ADD CONSTRAINT ed25519_pubkey_len
    CHECK (ed25519_pubkey IS NULL OR octet_length(ed25519_pubkey) = 32);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
DO $$ BEGIN
  ALTER TABLE device_auth ADD CONSTRAINT session_secret_len
    CHECK (session_secret IS NULL OR octet_length(session_secret) = 20);
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

-- one pending challenge nonce per device (single-use; short TTL enforced in the handler)
CREATE TABLE IF NOT EXISTS c2_nonce (
  serial    TEXT PRIMARY KEY REFERENCES devices(serial) ON DELETE CASCADE,
  nonce     BYTEA NOT NULL,
  issued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT c2_nonce_len CHECK (octet_length(nonce) = 16)
);
