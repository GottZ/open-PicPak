-- open-picpak backend - schema v4 rollback.
DROP TABLE IF EXISTS c2_nonce;
ALTER TABLE device_auth
  DROP CONSTRAINT IF EXISTS ecdsa_pubkey_len,
  DROP CONSTRAINT IF EXISTS session_secret_len,
  DROP COLUMN IF EXISTS ecdsa_pubkey,
  DROP COLUMN IF EXISTS session_secret,
  DROP COLUMN IF EXISTS session_epoch,
  DROP COLUMN IF EXISTS session_last_counter,
  DROP COLUMN IF EXISTS session_bootstrapped;
