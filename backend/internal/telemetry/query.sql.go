package telemetry

// The exact SQL strings, isolated here as the single audit point (design 08 §8 / D22.1: keep the column
// list in one place so schema drift or a hardcoded column is caught at review). Every statement is a
// SELECT — this package is read-only (D22.1). Column names/order match 0001_init.up.sql exactly.

// fleetSQL is read-shape 1, the enriched fleet list (D22.2/D22.3). It is driven by `devices` (the
// registration truth) LEFT JOIN LATERAL each device's newest telemetry row — NOT `DISTINCT ON (serial)
// FROM telemetry`, which would DROP every device that has never pushed (i.e. every device today, the
// empty-table normal §2) and show an empty fleet. A registered-but-silent device survives here with NULL
// telemetry columns → HasData=false → NO_DATA (T1). It also selects the two freshness clocks: devices.
// last_seen (the data clock) and device_auth.last_seen_at AS c2_last_seen (the liveness clock, D22.12 —
// device_auth is already joined for `bonded`, so the second clock is free). $1 is an optional serial
// filter (empty array → all devices): the SSE producer (W3) reuses this query to enrich only the changed
// serials. extra/rssi are NOT selected here — they live in the per-device detail, never the fleet snapshot
// (§2). Rides telemetry_serial_time (serial, time DESC), the access pattern the schema was built for.
const fleetSQL = `SELECT
    d.serial, d.label, d.channel AS assigned_channel, d.last_seen AS reg_last_seen,
    COALESCE(da.session_bootstrapped, false) AS bonded, da.last_seen_at AS c2_last_seen,
    t.time, t.batt_mv, t.batt_pct, t.bad_boots, t.boot_count, t.reset_reason, t.usb,
    t.uptime_ms, t.running_ver, t.channel AS reported_channel, t.diag_ota_rr
FROM devices d
LEFT JOIN device_auth da ON da.serial = d.serial
LEFT JOIN LATERAL (
    SELECT * FROM telemetry te WHERE te.serial = d.serial ORDER BY te.time DESC LIMIT 1
) t ON true
WHERE (cardinality($1::text[]) = 0 OR d.serial = ANY($1))
ORDER BY d.serial
LIMIT $2`

// latestDeviceSQL is read-shape 2a: the newest FULL row for one serial, including the diag_* OTA fields
// and the extra JSONB (rssi/heap/temp/tx read opportunistically here). Hits telemetry_serial_time.
const latestDeviceSQL = `SELECT
    serial, time, batt_mv, batt_pct, bad_boots, boot_count, reset_reason,
    usb, uptime_ms, running_ver, channel,
    diag_run_part, diag_runstate, diag_o0, diag_o1, diag_inv, diag_boots,
    diag_rr, diag_ota_rr, diag_mv, diag_mv_err, diag_stage, extra
FROM telemetry
WHERE serial = $1
ORDER BY time DESC
LIMIT 1`

// deviceHistorySQL is read-shape 2b: the recent history window for one serial, newest-first, DOUBLE
// bounded by the time window ($2) AND the row cap ($3, history_max_rows — D22.8, bounds memory + render).
// WHERE serial = $1 AND time >= $2 walks telemetry_serial_time. diag_ota_rr is selected so the card can
// chip a brownout-during-OTA per point; uptime_ms + reset_reason feed the cross-row rollback chip.
const deviceHistorySQL = `SELECT
    time, batt_mv, batt_pct, bad_boots, boot_count, reset_reason, uptime_ms, diag_ota_rr
FROM telemetry
WHERE serial = $1 AND time >= $2
ORDER BY time DESC
LIMIT $3`
