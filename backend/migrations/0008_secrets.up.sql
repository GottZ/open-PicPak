-- 0008: sealed secrets KV (A18 / M7) — AES-256-GCM ciphertext at rest. The master key (SECRETS_KEY)
-- is env-only and never reaches this table: the key that unseals DB rows cannot itself live in the DB.
-- Single-tenant: name is the natural key. Idempotent (IF NOT EXISTS), re-runnable against the final
-- schema. Append after 0007 in the compose `migrate` -f list (one list is the source of truth).
-- // TODO(multi-tenant): re-add a `scope` column and switch PRIMARY KEY -> (name, scope); restore
-- // the 4-part AAD/break-glass layout (foundation M2 seam).
CREATE TABLE IF NOT EXISTS secrets (
    name        TEXT        PRIMARY KEY,        -- resolve-by-name; PRIMARY KEY *is* UNIQUE(name)
    ciphertext  BYTEA       NOT NULL,           -- AES-256-GCM output incl. 16-byte auth tag
    nonce       BYTEA       NOT NULL,           -- 12 bytes, fresh per seal (GCM standard size)
    key_version INT         NOT NULL DEFAULT 1, -- master-key generation; bumped by the rotation sweep
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at  TIMESTAMPTZ,                    -- set when PUT overwrites an existing name
    CONSTRAINT  secrets_nonce_len CHECK (octet_length(nonce) = 12)
);
