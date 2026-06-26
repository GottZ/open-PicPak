-- open-picpak backend - schema v2 rollback: restore the v1 HOTP digits default.

ALTER TABLE device_auth ALTER COLUMN digits SET DEFAULT 6;
