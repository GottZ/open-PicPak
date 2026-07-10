-- 0013_playlist_render.down.sql — drop the W3 render-anchor tables. None is referenced by a later
-- migration, and device_playlist_binding's FK CASCADE on playlist is dropped with the table, so the
-- order is free; drop in reverse creation order for symmetry.
DROP TABLE IF EXISTS frame_variant_cache;
DROP TABLE IF EXISTS playlist_cursor;
DROP TABLE IF EXISTS device_playlist_binding;
