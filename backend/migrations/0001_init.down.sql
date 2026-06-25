-- open-picpak backend - schema v1 (wave S5a) - DOWN migration (rollback).
-- NEVER auto-applied; only run deliberately by the migration tool or by hand.
-- CASCADE drops the hypertables' chunks + the columnstore/retention jobs with the tables.

DROP TABLE IF EXISTS logs, telemetry, rollout_targets, device_log_fragment, device_log_cursor,
    device_auth, devices, channels, firmware_versions CASCADE;
