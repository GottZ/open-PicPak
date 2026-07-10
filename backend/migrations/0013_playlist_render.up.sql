-- 0013_playlist_render.up.sql — device→playlist binding, per-serial rotation cursor,
-- fleet-shared content-addressed packed-frame cache (A27 W3). Policy=Data.
-- Idempotent (IF NOT EXISTS) + independent + re-runnable against the final schema.
-- Mutual exclusion vs device_render_binding (0009) is enforced in code under
-- pg_advisory_xact_lock(hashtext(serial)) — a serial never holds a function AND a playlist
-- binding at once (§3/§5, faasstore.BindDevice + plrender.BindPlaylist).
CREATE TABLE IF NOT EXISTS device_playlist_binding (
    serial      TEXT   PRIMARY KEY,                          -- no FK (parity device_render_binding, 0009:26)
    playlist_id BIGINT NOT NULL REFERENCES playlist(id) ON DELETE CASCADE,
    bound_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS device_playlist_binding_pl_idx ON device_playlist_binding (playlist_id);

-- per-serial rotation state: which step is currently shown + when it was last advanced. position is
-- the 0-based INDEX into the deterministic rotation order (§4.4), clamped into range when the item
-- set shrinks. A re-bind resets the cursor (playlist_id is denormalised for that reset).
CREATE TABLE IF NOT EXISTS playlist_cursor (
    serial       TEXT   PRIMARY KEY,
    playlist_id  BIGINT NOT NULL,
    position     INT    NOT NULL DEFAULT 0,
    advanced_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- fleet-shared packed frame, keyed purely by CONTENT: same bytes+fit+dither pack identically for
-- every device → pack once, serve N. Content-addressed ⇒ self-invalidating (new bytes = new sha).
-- created_at is Observability only; GC is refcount-driven (NOT a created_at TTL), §6 / W6.
CREATE TABLE IF NOT EXISTS frame_variant_cache (
    image_sha   TEXT   NOT NULL,                             -- image.sha256
    fit         TEXT   NOT NULL,
    dither      TEXT   NOT NULL,
    packed      BYTEA  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (image_sha, fit, dither),
    CONSTRAINT frame_variant_len CHECK (octet_length(packed) = 30000)  -- parity 0009:44, ingest/frames.go:16
);
