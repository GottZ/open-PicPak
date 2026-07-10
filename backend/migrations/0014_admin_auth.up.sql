-- 0014_admin_auth.up.sql — admin authentication schema (A28 W1): machine bearer tokens,
-- password accounts, browser sessions, and an append-only audit trail. E28.1=(a) (password
-- accounts) is board-decided, so admin_users IS included here. Idempotent (IF NOT EXISTS) +
-- independent + re-runnable against the final schema (masterplan K1 doctrine, parity 0007/0011).
-- The only cross-table dependency is api_tokens.created_by -> operator_keys (0007); this migration
-- does not touch or reorder 0012/0013 (they belong to A27 and need not exist for 0014 to apply).

-- api_tokens: the machine-auth class, kept SEPARATE from operator_keys because a leaked image
-- token must NEVER carry the fleet-admin / RCE-enqueue capability (design §5 B3). token_id is a
-- public, indexable lookup handle; the secret is compared app-side in constant time against
-- secret_hash (design §3.1 — the S8 fetch-then-ConstantTimeCompare pattern, not the 0007
-- index-equality pattern). The plaintext token is NEVER stored. The token_id UNIQUE constraint
-- already yields the full index the hot-path lookup needs, so no extra partial index (design §6).
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    token_id     TEXT   NOT NULL UNIQUE,          -- public, non-secret lookup handle (prefix part)
    secret_hash  BYTEA  NOT NULL,                 -- sha256(secret); plaintext is NEVER stored
    label        TEXT   NOT NULL,
    scopes       TEXT[] NOT NULL DEFAULT '{}',    -- e.g. {'image:read','image:write'}
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,                     -- NULL = no expiry; otherwise a hard cutoff
    last_used_at TIMESTAMPTZ,
    disabled_at  TIMESTAMPTZ,                     -- soft revoke (SEC-M1); auth requires it IS NULL
    created_by   BIGINT REFERENCES operator_keys(id),  -- mint attribution (parity 0007:21-22)
    CONSTRAINT api_tokens_secret_hash_len CHECK (octet_length(secret_hash) = 32),
    CONSTRAINT api_tokens_scopes_known CHECK (scopes <@ ARRAY['image:read','image:write']::text[])
);

-- admin_users: password accounts (E28.1=(a)). password_hash is an argon2id PHC string
-- (self-describing salt + params); plaintext is NEVER stored. disabled_at soft-disables an
-- account — verify fails even with the correct password (design delta-4 §Design).
CREATE TABLE IF NOT EXISTS admin_users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username      TEXT   NOT NULL UNIQUE,
    password_hash TEXT   NOT NULL,                -- argon2id PHC string
    is_admin      BOOLEAN NOT NULL DEFAULT false, -- same role-vs-admin semantics as operator_keys
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at   TIMESTAMPTZ
);

-- admin_sessions: browser sessions. id = sha256(cookie-secret); the cookie carries the plaintext
-- secret, so a DB leak yields no usable cookies (hash-at-rest, same principle as the tokens).
-- expires_at is the server-authoritative absolute lifetime.
CREATE TABLE IF NOT EXISTS admin_sessions (
    id           BYTEA PRIMARY KEY,               -- sha256(session-secret)
    user_id      BIGINT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,            -- absolute lifetime (server-authoritative)
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT admin_sessions_id_len CHECK (octet_length(id) = 32)
);
CREATE INDEX IF NOT EXISTS admin_sessions_expiry ON admin_sessions (expires_at);

-- admin_audit: append-only trail (design §4.6). After the SSO removal (W8) admin is public, so a
-- who/what/when for mint/revoke/image-delete/login is load-bearing, not optional. NEVER stores a
-- plaintext secret in target (only token_id / username / image-id).
CREATE TABLE IF NOT EXISTS admin_audit (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_kind  TEXT NOT NULL,                     -- 'session' | 'bearer' | 'operator' | 'cli' | 'basic'
    actor_id    TEXT,                              -- user_id / api_token.token_id / operator_key.id
    action      TEXT NOT NULL,                     -- 'token.mint' | 'token.revoke' | 'image.delete' | 'session.login' | ...
    target      TEXT,                              -- image-id / token-id / username (NO plaintext secret)
    remote_ip   TEXT,                              -- the non-spoofable source fixed in design §4.4
    ok          BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS admin_audit_at ON admin_audit (at DESC);
