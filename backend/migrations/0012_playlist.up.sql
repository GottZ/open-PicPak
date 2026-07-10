-- 0012_playlist.up.sql — playlist + ordered items (A27 W2). Policy=Data.
-- Idempotent (IF NOT EXISTS) + independent + re-runnable against the final schema.
-- Delta 3 (design/33 §3 + delta-2-ondevice-rotation): playlist.managed_serial carries the
-- "Aufs Panel" shortcut key — the ~single/ naming convention is unbuildable against the name
-- regex (27:90, '~'/'/'/uppercase illegal) and would clobber on serial case-folding; a column
-- is the only sound key. ListPlaylists filters managed rows by default (operator lists without
-- management noise); an IncludeManaged option serves internal callers.
-- // TODO(multi-tenant): name -> UNIQUE(operator_key_id, name); GLOBAL-unique leaks foreign
-- // names via 409 (§5, K-MT). Same seam as image.sha256 (0011).
CREATE TABLE IF NOT EXISTS playlist (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    operator_key_id BIGINT REFERENCES operator_keys(id),      -- attribution, FK RESTRICT (parity 0007:21-22)
    name            TEXT   NOT NULL UNIQUE,                    -- operator-chosen; ^[a-z0-9][a-z0-9._-]{0,127}$ (Go-side ValidName)
    interval_s      INT    NOT NULL DEFAULT 900,               -- rotation cadence; clamped >=60 at serve (wake floor)
    order_mode      TEXT   NOT NULL DEFAULT 'sequential'
                        CHECK (order_mode IN ('sequential','shuffle')),
    shuffle_epoch   INT    NOT NULL DEFAULT 1,                 -- shuffle permutation seed input; bumped ONLY by explicit reshuffle, NOT item mutations (§4.4)
    version         INT    NOT NULL DEFAULT 1,                 -- bumped on ANY playlist/item mutation (selection cache-bust, K7 parity)
    managed_serial  TEXT   UNIQUE,                             -- Delta 3: "Aufs Panel" shortcut upsert key; NULL for operator-authored playlists
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT playlist_interval_pos CHECK (interval_s > 0)
);

CREATE TABLE IF NOT EXISTS playlist_item (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    playlist_id BIGINT NOT NULL REFERENCES playlist(id) ON DELETE CASCADE,   -- delete playlist → drop items
    image_id    BIGINT NOT NULL REFERENCES image(id),                        -- FK RESTRICT: an image in use can't be deleted (23503 → 409)
    position    INT    NOT NULL,                                             -- ordering; gap-tolerant integers (Reorder renumbers)
    fit         TEXT   NOT NULL DEFAULT 'cover'
                    CHECK (fit IN ('cover','contain','fill')),               -- sharp resize fit (worker)
    dither      TEXT   NOT NULL DEFAULT 'none'
                    CHECK (dither IN ('none','floyd-steinberg','atkinson','ordered')),  -- policy only; algo = A31
    UNIQUE (playlist_id, position)
);
CREATE INDEX IF NOT EXISTS playlist_item_playlist_idx ON playlist_item (playlist_id, position);
CREATE INDEX IF NOT EXISTS playlist_item_image_idx    ON playlist_item (image_id, fit, dither);   -- FK-RESTRICT delete check (image_id prefix) + variant-cache refcount GC (§6)
