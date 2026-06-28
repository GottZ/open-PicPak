-- open-picpak backend - schema v4 rollback.
DROP TABLE IF EXISTS c2_nonce;
ALTER TABLE device_auth
  DROP CONSTRAINT IF EXISTS ed25519_pubkey_len,
  DROP CONSTRAINT IF EXISTS session_secret_len,
  DROP COLUMN IF EXISTS ed25519_pubkey,
  DROP COLUMN IF EXISTS session_secret,
  DROP COLUMN IF EXISTS session_epoch,
  DROP COLUMN IF EXISTS session_last_counter,
  DROP COLUMN IF EXISTS session_bootstrapped;
