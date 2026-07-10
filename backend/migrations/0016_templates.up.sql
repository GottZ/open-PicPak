-- 0016_templates.up.sql — template store (A30 W1). Policy=Data: template content, parameter schema
-- and the render_fn trust profile are ROWS, not code constants. Idempotent (IF NOT EXISTS) +
-- independent + re-runnable against the final schema (parity 0009:2-3, 0011:2).
CREATE TABLE IF NOT EXISTS templates (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,                    -- Go-mirror charset ^[a-z0-9][a-z0-9._-]{0,127}$ (templatestore.ValidName);
                                                         -- builtin rows live in the reserved 'builtin/' namespace, which ValidName forbids operators
    kind        TEXT NOT NULL
                  CHECK (kind IN ('render_fn','berry_snippet','playlist_preset')),
    source      TEXT NOT NULL,                           -- JS (render_fn) | Berry (berry_snippet) | JSON doc (playlist_preset)
    params      JSONB NOT NULL DEFAULT '[]'::jsonb,      -- parameter schema: [{name,label,type,default,required,options}]
    -- render_fn trust profile (A24 least-privilege): which egress host + which secrets the applied
    -- faas_functions row inherits. Fail-closed: empty = no host, no secret. Only names/hosts, never
    -- plaintext secret values (parity faas_functions.secret_bindings, §5.4).
    egress_allow    TEXT[]  NOT NULL DEFAULT '{}',       -- passed to faasstore.CreateParams.EgressAllow on apply
    secret_bindings TEXT[]  NOT NULL DEFAULT '{}',       -- passed to faasstore.CreateParams.SecretBindings on apply
    trigger_config  JSONB,                               -- passed to faasstore.CreateParams.TriggerConfig (nullable)
    builtin     BOOLEAN NOT NULL DEFAULT false,          -- prefab, seeded from the embedded catalog (startup upsert, never a hand seed)
    version     INT NOT NULL DEFAULT 1,                  -- bumped on a real source/params UPDATE (editor history / seed refresh)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Per-kind cap: a berry_snippet MUST fit under the effective firmware cap (K3 poison-pill).
    -- octet_length (BYTES), NOT char_length: the firmware cap C2_RESP_MAX-1 = 8191 is byte-based
    -- (net.c:686 memcpy of a byte data_len). char_length counts codepoints, so a multibyte source
    -- (umlaut / emoji / string literal) with char_length<=8191 could still ship >8191 B = the poison
    -- pill the CHECK exists to prevent. Byte-genau is load-bearing (mirror commandstore.ScriptMax).
    CONSTRAINT tmpl_berry_len CHECK (kind <> 'berry_snippet' OR octet_length(source) <= 8191)
);
-- Composite (kind, id) covers the kind picker AND the kind-filtered keyset walk
-- (WHERE kind=$1 AND id>$after ORDER BY id) with one index range scan; a single-column kind index
-- would have to re-sort.
CREATE INDEX IF NOT EXISTS templates_kind_id_idx ON templates (kind, id);

-- Provenance stamp: which faas_functions row an apply instantiated from which template (nullable =
-- hand-written). ON DELETE SET NULL: deleting a template never severs the running function (templates
-- are blueprints, functions are independent copies from the moment of apply, §3.2).
ALTER TABLE faas_functions ADD COLUMN IF NOT EXISTS template_id BIGINT
    REFERENCES templates(id) ON DELETE SET NULL;
-- FK columns are NOT auto-indexed. ON DELETE SET NULL scans the child on every template delete →
-- a seq scan per delete without this index.
CREATE INDEX IF NOT EXISTS faas_functions_template_id_idx ON faas_functions (template_id);
