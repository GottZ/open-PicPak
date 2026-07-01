-- 0009_faas.down.sql — drop children (FK) first, then the parent.
DROP TABLE IF EXISTS faas_frame_lastgood;
DROP TABLE IF EXISTS device_render_binding;
DROP TABLE IF EXISTS faas_functions;
