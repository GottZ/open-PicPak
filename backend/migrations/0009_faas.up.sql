-- 0009_faas.up.sql — FaaS function store + per-device render binding + durable last-good (A24).
-- Policy=Data: trigger model, cadence, dither, egress, secret bindings are ROWS, not code.
-- Idempotent (IF NOT EXISTS) + independent DDL, re-runnable against the final schema.
-- // TODO(multi-tenant): add an owner/tenant column + composite keys (foundation M2 seam).

CREATE TABLE IF NOT EXISTS faas_functions (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT        NOT NULL UNIQUE,                 -- operator-chosen; ^[a-z0-9][a-z0-9._-]{0,127}$ (Go-side)
    source          TEXT        NOT NULL,                        -- the function JS (foreign code)
    version         INT         NOT NULL DEFAULT 1,              -- bumped on each source UPDATE (rollback-able, K7 cache-bust)
    trigger_type    TEXT        NOT NULL DEFAULT 'render'
                        CHECK (trigger_type IN ('render','schedule','webhook')),
    trigger_config  JSONB       NOT NULL DEFAULT '{}'::jsonb,    -- render:{mode,ttl_s,interval_s,dither,cadence} schedule:{cron} webhook:{}
    webhook_token_sha256 BYTEA,                                  -- sha256(token); webhook triggers only. Shown ONCE at create
                                                                 -- (D24.13/D17.2); NEVER stored plaintext, NEVER in trigger_config.
    secret_bindings TEXT[]      NOT NULL DEFAULT '{}',           -- names the fn may receive (least privilege, D24.4)
    egress_allow    TEXT[]      NOT NULL DEFAULT '{}',           -- host[:port] allowlist; enforced at the egress proxy (D24.8)
    enabled         BOOLEAN     NOT NULL DEFAULT false,          -- default-off (pausability)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT      faas_webhook_token_len CHECK (webhook_token_sha256 IS NULL OR octet_length(webhook_token_sha256) = 32)
);

-- which render function a device shows. No FK on serial: a binding may name a device row that is
-- re-onboarded later, and device-delete clears it in code (parity with rollout_targets, D20.10).
CREATE TABLE IF NOT EXISTS device_render_binding (
    serial      TEXT   PRIMARY KEY,
    function_id BIGINT NOT NULL REFERENCES faas_functions(id) ON DELETE CASCADE,
    bound_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- reverse lookup for cron/prerender fan-out over every serial bound to a function (D24.12).
CREATE INDEX IF NOT EXISTS device_render_binding_fn_idx ON device_render_binding (function_id);

-- durable last-good frame per (serial, function): survives a supervisor restart (boot-reload).
-- 30000-byte packed BWRY buffer.
CREATE TABLE IF NOT EXISTS faas_frame_lastgood (
    serial      TEXT   NOT NULL,
    function_id BIGINT NOT NULL REFERENCES faas_functions(id) ON DELETE CASCADE,
    packed      BYTEA  NOT NULL,
    rendered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status      TEXT   NOT NULL DEFAULT 'ok',
    PRIMARY KEY (serial, function_id),
    CONSTRAINT  faas_lastgood_len CHECK (octet_length(packed) = 30000)
);
