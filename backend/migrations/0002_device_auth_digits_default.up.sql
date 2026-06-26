-- open-picpak backend - schema v2: HOTP digits reconciliation.
-- Protocol default is 8 digits. Existing v1 rows with the old default are upgraded in place.

ALTER TABLE device_auth ALTER COLUMN digits SET DEFAULT 8;
UPDATE device_auth SET digits = 8 WHERE digits = 6;
