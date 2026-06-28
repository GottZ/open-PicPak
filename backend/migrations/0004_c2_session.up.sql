-- open-picpak backend - schema v4 (Doc 15): C2 auth re-architecture.
-- Long-term asymmetric device identity (the backend holds ONLY the public key) + a per-session HOTP
-- secret established via the signed re-key handshake. Decouples the HOTP counter from boot_count
-- (kills the bc-lockout) and removes the server-held shared secret. Primitive: ECDSA P-256 (secp256r1)
-- — Ed25519 is NOT implemented in ESP-IDF mbedTLS, ECDSA P-256 is (and HW-capable). The legacy
-- hotp_secret/bc path stays during the bridge window and is dropped at cutover. Idempotent.

-- A bonded device is identified by its public key alone (no shared secret) -> hotp_secret nullable.
-- Guarded so this migration stays re-runnable AFTER 0005 dropped hotp_secret: the deploy re-applies
-- every migration in order, so each must be idempotent against the FINAL schema, not just its own era.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_name = 'device_auth' AND column_name = 'hotp_secret') THEN
    ALTER TABLE device_auth ALTER COLUMN hotp_secret DROP NOT NULL;
  END IF;
END $$;

-- ed25519_pubkey from an earlier draft of this migration is superseded by ecdsa_pubkey.
ALTER TABLE device_auth DROP CONSTRAINT IF EXISTS ed25519_pubkey_len;
ALTER TABLE device_auth DROP COLUMN IF EXISTS ed25519_pubkey;

ALTER TABLE device_auth
  ADD COLUMN IF NOT EXISTS ecdsa_pubkey         BYTEA,   -- 65 B uncompressed P-256 (0x04||X||Y); NULL until bonded
  ADD COLUMN IF NOT EXISTS session_secret       BYTEA,   -- 20 B; NULL until first re-key
  ADD COLUMN IF NOT EXISTS session_epoch        BIGINT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS session_last_counter BIGINT  NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS session_bootstrapped BOOLEAN NOT NULL DEFAULT false;

DO $$ BEGIN
  ALTER TABLE device_auth ADD CONSTRAINT ecdsa_pubkey_len
    CHECK (ecdsa_pubkey IS NULL OR octet_length(ecdsa_pubkey) = 65);
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
