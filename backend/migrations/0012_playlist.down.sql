-- 0012_playlist.down.sql — drop playlist_item first (its FK RESTRICT on image pins the row),
-- then playlist, so 0011.down can DROP image afterwards. ON DELETE CASCADE on playlist_item
-- makes the playlist drop self-clearing, but the explicit item drop keeps the teardown order
-- independent of cascade behaviour.
DROP TABLE IF EXISTS playlist_item;
DROP TABLE IF EXISTS playlist;
