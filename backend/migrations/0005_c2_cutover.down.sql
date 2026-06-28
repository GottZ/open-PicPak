-- open-picpak backend - schema v5 rollback (best-effort; legacy columns restored empty/nullable).
ALTER TABLE device_auth
  ADD COLUMN IF NOT EXISTS hotp_secret      BYTEA,
  ADD COLUMN IF NOT EXISTS last_boot_count  BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS last_rtc_counter BIGINT NOT NULL DEFAULT -1;
