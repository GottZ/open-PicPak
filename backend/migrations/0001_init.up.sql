-- open-picpak backend - schema v1 (wave S5a) - FORWARD migration.
-- Base schema for the backend data model and firmware HTTP/OTA contract.
-- golang-migrate convention (.up.sql / .down.sql); applied forward-only; NEVER auto-runs the down.
-- Idempotent (IF NOT EXISTS / if_not_exists) so re-application is safe.
-- Policy as data: all intervals are migration/config values, not code constants.
-- Later waves wire Go ingest/admin code to these tables; this migration creates the base topology.

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- control tables (relational)
-- firmware_versions first (no FK); channels references it; devices references channels.
CREATE TABLE IF NOT EXISTS firmware_versions (
    version     TEXT PRIMARY KEY,                 -- == X-Firmware-Version (strncmp, ota.c:146)
    sha256      TEXT NOT NULL,                    -- == X-Firmware-SHA256 (fail-closed, ota.c:223)
    blob_path   TEXT NOT NULL,                    -- path/key of firmware.bin in the volume/object store
    size_bytes  BIGINT NOT NULL,
    notes       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- version: max 31 chars; esp_app_desc_t.version[32] (strncmp window, ota.c:146).
    CONSTRAINT version_len CHECK (char_length(version) <= 31),
    -- sha256: EXACTLY 64 LOWERCASE hex. FW compares with strcmp (NOT strcasecmp), ota.c:339-340.
    CONSTRAINT sha256_hex  CHECK (sha256 ~ '^[0-9a-f]{64}$')
);

CREATE TABLE IF NOT EXISTS channels (
    name            TEXT PRIMARY KEY,             -- 'stable' | 'beta' | etc. (policy as data)
    default_version TEXT REFERENCES firmware_versions(version)
);
INSERT INTO channels (name) VALUES ('stable'), ('beta') ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS devices (
    serial            TEXT PRIMARY KEY,           -- D8: public identifier (dev_sn)
    channel           TEXT NOT NULL DEFAULT 'stable' REFERENCES channels(name),
    mac               TEXT,                        -- cross-ref (migration from id=MAC-suffix)
    label             TEXT,                        -- human name (location etc.)
    first_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen         TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS device_auth (
    serial            TEXT PRIMARY KEY REFERENCES devices(serial) ON DELETE CASCADE,
    hotp_secret       BYTEA NOT NULL,               -- per-device secret, NEVER in response/log
    prev_secret       BYTEA,                        -- rotation window support
    prev_valid_until  TIMESTAMPTZ,
    rtc_bits          SMALLINT NOT NULL DEFAULT 24,
    digits            SMALLINT NOT NULL DEFAULT 6,
    last_boot_count   BIGINT NOT NULL DEFAULT 0,
    last_rtc_counter  BIGINT NOT NULL DEFAULT -1,
    fail_count        INTEGER NOT NULL DEFAULT 0,
    locked_until      TIMESTAMPTZ,
    provisioned_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at      TIMESTAMPTZ,
    CONSTRAINT hotp_secret_len CHECK (octet_length(hotp_secret) = 20),
    CONSTRAINT rtc_bits_range CHECK (rtc_bits BETWEEN 1 AND 32),
    CONSTRAINT digits_range CHECK (digits BETWEEN 6 AND 8)
);
-- Secret rotation MUST reset the high-water mark atomically in the admin write layer.

CREATE TABLE IF NOT EXISTS device_log_cursor (
    serial       TEXT PRIMARY KEY REFERENCES devices(serial) ON DELETE CASCADE,
    epoch        BIGINT NOT NULL DEFAULT 0,
    ack_off      BIGINT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS device_log_fragment (
    serial       TEXT NOT NULL REFERENCES devices(serial) ON DELETE CASCADE,
    epoch        BIGINT NOT NULL,
    start_off    BIGINT,
    end_off      BIGINT NOT NULL,
    payload      TEXT NOT NULL,
    gap          BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (serial, epoch, end_off)
);
CREATE INDEX IF NOT EXISTS device_log_fragment_serial_epoch ON device_log_fragment (serial, epoch, end_off);

CREATE TABLE IF NOT EXISTS rollout_targets (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    serial      TEXT NOT NULL,                    -- '*' = whole fleet of the channel, else SN (staging pin)
    channel     TEXT NOT NULL REFERENCES channels(name),
    version     TEXT NOT NULL REFERENCES firmware_versions(version),
    state       TEXT NOT NULL DEFAULT 'active',   -- 'active' | 'paused' | 'done'
    pinned      BOOLEAN NOT NULL DEFAULT false,   -- true = per-serial pin beats channel default
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (serial, channel)
);

-- hypertables (time series)
CREATE TABLE IF NOT EXISTS telemetry (
    time         TIMESTAMPTZ NOT NULL,            -- server time; device has no wall-clock
    serial       TEXT NOT NULL,
    batt_mv      INTEGER,                          -- v
    batt_pct     SMALLINT,                         -- p
    bad_boots    INTEGER,                          -- bad
    boot_count   BIGINT,                           -- bc (reset-proof, otadiag/boots)
    reset_reason TEXT,                              -- rr
    usb          BOOLEAN,                           -- usb (phase boundary)
    uptime_ms    BIGINT,                            -- up (uptime reset = reboot detector, analyze.py:36)
    running_ver  TEXT,                              -- fw
    channel      TEXT,                              -- ch
    -- parsed from X-Picpak-Diag (ota.c:400-403); all fields:
    diag_run_part TEXT,
    diag_runstate SMALLINT,
    diag_o0       SMALLINT, diag_o1 SMALLINT,
    diag_inv      TEXT,
    diag_boots    BIGINT,
    diag_rr       SMALLINT,
    diag_ota_rr   SMALLINT,                         -- BROWNOUT-during-OTA (only indicator!)
    diag_mv       BIGINT,
    diag_mv_err   SMALLINT,
    diag_stage    TEXT,
    extra        JSONB,                              -- F2 metric growth without schema migration
    src_ip       INET
);
SELECT create_hypertable('telemetry', by_range('time', INTERVAL '7 days'), if_not_exists => true);
CREATE INDEX IF NOT EXISTS telemetry_serial_time ON telemetry (serial, time DESC);

CREATE TABLE IF NOT EXISTS logs (
    time         TIMESTAMPTZ NOT NULL,
    serial       TEXT NOT NULL,
    boot_count   BIGINT,                            -- bc; boot epoch (gap discriminator)
    seq          BIGINT,                            -- monotone server sequence per device
    offset_start BIGINT,                            -- device-claimed raw ring offset (D9; NULL on ota-snapshot)
    payload      TEXT NOT NULL,                     -- X-Picpak-Log ('|' == newline, logbuf.c:86)
    source       TEXT NOT NULL DEFAULT 'telemetry', -- 'telemetry' (off-bearing) | 'ota-snapshot'
    gap          BOOLEAN NOT NULL DEFAULT false,    -- bc-difference>0 = new boot epoch before
    suspect      BOOLEAN NOT NULL DEFAULT false     -- offset rollback WITHOUT bc increment (replay/bug)
);
SELECT create_hypertable('logs', by_range('time', INTERVAL '7 days'), if_not_exists => true);
CREATE INDEX IF NOT EXISTS logs_serial_time ON logs (serial, time DESC);

-- columnstore (Hypercore, TimescaleDB 2.18+) + retention; policy as data, idempotent
-- NOTE: add_columnstore_policy is a PROCEDURE in TimescaleDB (verified pg_proc.prokind='p') -> CALL;
--       add_retention_policy is a FUNCTION -> SELECT.
ALTER TABLE telemetry SET (timescaledb.enable_columnstore = true,
                           timescaledb.segmentby = 'serial',
                           timescaledb.orderby   = 'time DESC');
CALL   add_columnstore_policy('telemetry', after => INTERVAL '14 days', if_not_exists => true);
SELECT add_retention_policy  ('telemetry', drop_after => INTERVAL '365 days', if_not_exists => true);

ALTER TABLE logs SET (timescaledb.enable_columnstore = true,
                      timescaledb.segmentby = 'serial', timescaledb.orderby = 'time DESC');
CALL   add_columnstore_policy('logs', after => INTERVAL '7 days', if_not_exists => true);
SELECT add_retention_policy  ('logs',  drop_after => INTERVAL '90 days', if_not_exists => true);
