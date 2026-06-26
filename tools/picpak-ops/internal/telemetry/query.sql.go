package telemetry

// The exact SQL strings, isolated here as the single audit point (design 08 §8 risk
// mitigation: keep the column list in one place so schema drift / a hardcoded column
// is caught at review). Every statement is a SELECT — this axis is read-only and the
// shared pool is read_only-enforced (internal/db), so a stray write would be rejected
// with SQLSTATE 25006 anyway. Column names/order match 0001_init.up.sql exactly.

// fleetSQL is read-shape 1: one latest row per device. DISTINCT ON (serial) +
// ORDER BY serial, time DESC takes the newest row per serial in a single round-trip;
// LIMIT bounds the device count (fleet_max_devices safety cap). It rides the
// telemetry_serial_time (serial, time DESC) index — the access pattern the schema was
// built for. There is NO rssi column; rssi lives only in extra JSONB (read in the
// per-device detail, not the fleet snapshot).
const fleetSQL = `SELECT DISTINCT ON (serial)
    serial, time, batt_mv, batt_pct, bad_boots, boot_count, reset_reason,
    usb, uptime_ms, running_ver, channel, diag_ota_rr
FROM telemetry
ORDER BY serial, time DESC
LIMIT $1`

// latestDeviceSQL is read-shape 2a: the newest FULL row for one serial, including the
// diag_* OTA fields and the extra JSONB. Hits the telemetry_serial_time index.
const latestDeviceSQL = `SELECT
    serial, time, batt_mv, batt_pct, bad_boots, boot_count, reset_reason,
    usb, uptime_ms, running_ver, channel,
    diag_run_part, diag_runstate, diag_o0, diag_o1, diag_inv, diag_boots,
    diag_rr, diag_ota_rr, diag_mv, diag_mv_err, diag_stage, extra
FROM telemetry
WHERE serial = $1
ORDER BY time DESC
LIMIT 1`

// deviceHistorySQL is read-shape 2b: the recent history window for one serial,
// newest-first, capped at history_max_rows. WHERE serial = $1 AND time >= $2 walks
// the telemetry_serial_time index. Only the plottable/trend columns are selected.
const deviceHistorySQL = `SELECT
    time, batt_mv, batt_pct, bad_boots, boot_count, reset_reason, uptime_ms
FROM telemetry
WHERE serial = $1 AND time >= $2
ORDER BY time DESC
LIMIT $3`
