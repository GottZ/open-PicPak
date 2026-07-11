-- 0019_template_preview.up.sql — durable template preview cache (W-A33.5a, design 33 §3 Delta 2 +
-- §4.5a). Policy=Data: a rendered preview PNG is a ROW keyed by (template_id, version), never a
-- filesystem artefact. The cache is the storm-shield (§6): a gallery cold-visit ALWAYS hits this table
-- for builtins (SeedBuiltins warms them with a 1-slot budget), so the 4 shared faas worker slots are
-- never contended by UI browsing. Durable = survives restart/deploy (a Deploy does not re-render the
-- whole catalog). ON DELETE CASCADE ties a preview's lifetime to its template (a deleted operator
-- template drops its cached previews with it). Idempotent (IF NOT EXISTS) + re-runnable against the
-- final schema (parity 0016:2-3).
CREATE TABLE IF NOT EXISTS template_preview (
    template_id BIGINT      NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    version     INT         NOT NULL,                    -- the templates.version the PNG was rendered from
    png         BYTEA       NOT NULL,                    -- 400x300 @ 4-colour BWRY paletted PNG (~2-10 KB)
    rendered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- (template_id, version) is the cache key: the preview route serves by it and 304s on the matching
    -- ETag; the version component makes a bumped template (seed refresh / operator edit) a cache miss
    -- that re-renders, never a stale serve.
    PRIMARY KEY (template_id, version)
);
