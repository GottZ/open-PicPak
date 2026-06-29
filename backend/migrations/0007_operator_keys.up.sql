-- 0007: operator control-plane (A17) — bearer keys for the admin API + RCE-route attribution.
-- Idempotent (IF NOT EXISTS), re-runnable against the final schema.
-- NOTE: appending this file to the compose `migrate` -f list ALSO repairs the verified omission of
-- 0004..0006 from that list (they exist on disk and were applied to the live DB, but the compose
-- entrypoint only listed 0001..0003 — one list is the source of truth).

CREATE TABLE IF NOT EXISTS operator_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_hash   BYTEA NOT NULL UNIQUE,          -- sha256(bearer); plaintext is NEVER stored
    label        TEXT NOT NULL,
    is_admin     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    disabled_at  TIMESTAMPTZ,                     -- soft revoke; auth requires disabled_at IS NULL
    CONSTRAINT token_hash_len CHECK (octet_length(token_hash) = 32)
);

-- Attribution for the RCE-capable enqueue route. Nullable: device-era rows and the '*' broadcast
-- backfill have no operator; the FK is RESTRICT (default) so an operator key cannot be hard-deleted
-- while it still owns queued commands (operator revoke is soft, via operator_keys.disabled_at).
ALTER TABLE command_queue
    ADD COLUMN IF NOT EXISTS operator_key_id BIGINT REFERENCES operator_keys(id);
