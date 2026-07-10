-- 0011_image.down.sql — drop the source-image store (W2's playlist_item FK RESTRICT is dropped first by 0012.down).
DROP TABLE IF EXISTS image;
