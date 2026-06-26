// Package telemetry is the read-only, per-device telemetry axis (W9 / design 08).
// It pulls rows from the TimescaleDB `telemetry` hypertable through the shared
// read-only *pgxpool.Pool (owned by internal/db, never opened here) and presents a
// per-device health view plus recent history/trend. It WRITES NOTHING.
//
// Telemetry is device-pushed and SPARSE: a row exists only when a device fetched a
// frame, so the view foregrounds last-seen AGE and never implies a live feed. The
// pane tolerates a nil pool (no DSN configured) by rendering a "no database
// configured" state, and an empty hypertable by rendering an explicit "no telemetry
// yet" state — neither is a crash nor a misleading all-`n/a` grid.
//
// This file holds the value types: the Null[T] wrapper, the Health verdict enum, the
// per-row structs the repository scans into, and the per-column sentinel constants.
package telemetry

import "time"

// Null[T] is a typed optional. Valid is false for a SQL NULL or a per-column
// sentinel (mapped at scan time in sentinel.go); a consumer renders an invalid Null
// as "n/a" and never reads V. This keeps "not measured this boot" (firmware
// 02-telemetry §3.5) out of the trend/verdict math.
type Null[T any] struct {
	V     T
	Valid bool
}

// Get returns the value and whether it is present.
func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// some wraps a present value.
func some[T any](v T) Null[T] { return Null[T]{V: v, Valid: true} }

// Per-column sentinels (design 08 §5; firmware 02-telemetry §3.2/§3.5). These are
// assigned PER METRIC by the firmware, so the reader maps column → its own sentinel
// set, never a blanket list applied to every integer column. Critically -127/-32768
// are bound ONLY to the JSONB rssi/temp metrics (which have no column), so a genuine
// -32768 in a SMALLINT diag_* column (range -32768..32767) is never masked.
const (
	// SentinelGeneric is the "not measured this boot" value for the generic numeric
	// columns uptime_ms, batt_mv, batt_pct (plus SQL NULL → n/a).
	SentinelGeneric int64 = -1
	// SentinelRSSI is the rssi sentinel — extra JSONB only (no column).
	SentinelRSSI int = -127
	// SentinelTemp is the temp sentinel — extra JSONB only (no column).
	SentinelTemp int = -32768
)

// Health is the per-device verdict (design 08 §2). Higher values are MORE severe;
// Verdict returns the most severe applicable verdict (OK when none apply).
type Health int

const (
	HealthOK       Health = iota // no condition triggered
	HealthLowBatt                // batt_pct <= low_batt_pct and (not USB | includes_usb)
	HealthBadBoots               // bad_boots >= bad_boots_warn (guard climbing to safe-mode)
	HealthBrownout               // brownout reset_reason / diag_ota_rr code / uptime-rollback
	HealthOffline                // now-time > stale_after (offline OR dead — unresolvable here)
)

// String renders the canonical verdict label used in the UI and tests.
func (h Health) String() string {
	switch h {
	case HealthOK:
		return "OK"
	case HealthLowBatt:
		return "LOW_BATT"
	case HealthBadBoots:
		return "BAD_BOOTS"
	case HealthBrownout:
		return "BROWNOUT"
	case HealthOffline:
		return "OFFLINE_STALE"
	default:
		return "UNKNOWN"
	}
}

// Sample is the minimal field set the verdict needs from one telemetry row. Both
// FleetRow and DeviceTelemetry/HistoryPoint expose a Sample(), so the verdict is
// computed identically from a fleet snapshot row or a detail row, with no DB import
// in health.go.
type Sample struct {
	Time        time.Time
	ResetReason Null[string]
	DiagOTARR   Null[int] // diag_ota_rr (SMALLINT, NULL-only sentinel)
	BadBoots    Null[int]
	BattPct     Null[int]
	USB         Null[bool]
	Uptime      Null[int64] // uptime_ms (-1 + NULL sentinel applied at scan)
}

// FleetRow is one device's latest telemetry, the DISTINCT ON (serial) snapshot that
// drives the device list/overview (design 08 §2 read-shape 1). DeviceChannel is the
// device-CLAIMED telemetry.channel; the authoritative channel comes from
// devices.channel via the fleet cache.
type FleetRow struct {
	Serial        string
	Time          time.Time
	BattMV        Null[int]
	BattPct       Null[int]
	BadBoots      Null[int]
	BootCount     Null[int64]
	ResetReason   Null[string]
	USB           Null[bool]
	Uptime        Null[int64]
	RunningVer    string
	DeviceChannel string // telemetry.channel (device-claimed)
	DiagOTARR     Null[int]
}

// Sample projects the verdict-relevant fields.
func (r FleetRow) Sample() Sample {
	return Sample{
		Time:        r.Time,
		ResetReason: r.ResetReason,
		DiagOTARR:   r.DiagOTARR,
		BadBoots:    r.BadBoots,
		BattPct:     r.BattPct,
		USB:         r.USB,
		Uptime:      r.Uptime,
	}
}

// DeviceTelemetry is the latest FULL row for the selected device (read-shape 2),
// including the diag_* OTA fields and the opportunistically-decoded extra JSONB
// metrics (rssi/heap/temp/tx).
type DeviceTelemetry struct {
	Serial        string
	Time          time.Time
	BattMV        Null[int]
	BattPct       Null[int]
	BadBoots      Null[int]
	BootCount     Null[int64]
	ResetReason   Null[string]
	USB           Null[bool]
	Uptime        Null[int64]
	RunningVer    string
	DeviceChannel string

	DiagRunPart  Null[string]
	DiagRunstate Null[int] // SMALLINT, NULL-only sentinel (real -32768 survives)
	DiagO0       Null[int] // SMALLINT, NULL-only sentinel
	DiagO1       Null[int] // SMALLINT, NULL-only sentinel
	DiagInv      Null[string]
	DiagBoots    Null[int64]
	DiagRR       Null[int]
	DiagOTARR    Null[int]
	DiagMV       Null[int64]
	DiagMVErr    Null[int]
	DiagStage    Null[string]

	Extra Extra // decoded extra JSONB (rssi/heap/temp/tx), all n/a when absent
}

// Sample projects the verdict-relevant fields.
func (d DeviceTelemetry) Sample() Sample {
	return Sample{
		Time:        d.Time,
		ResetReason: d.ResetReason,
		DiagOTARR:   d.DiagOTARR,
		BadBoots:    d.BadBoots,
		BattPct:     d.BattPct,
		USB:         d.USB,
		Uptime:      d.Uptime,
	}
}

// HistoryPoint is one row of the per-device history window, the series behind the
// trend sparklines and the recent-history table (read-shape 2). ResetReason +
// Uptime are kept so the uptime-rollback brownout heuristic can walk consecutive
// rows.
type HistoryPoint struct {
	Time        time.Time
	BattMV      Null[int]
	BattPct     Null[int]
	BadBoots    Null[int]
	BootCount   Null[int64]
	ResetReason Null[string]
	Uptime      Null[int64]
}

// Sample projects the verdict-relevant fields.
func (h HistoryPoint) Sample() Sample {
	return Sample{
		Time:        h.Time,
		ResetReason: h.ResetReason,
		BadBoots:    h.BadBoots,
		Uptime:      h.Uptime,
	}
}

// Extra is the opportunistically-decoded telemetry.extra JSONB (design 08 §1.1):
// rssi/heap/temp/tx live ONLY here (no column) so a future ingest can grow metrics
// without a schema migration. rssi/temp apply their own sentinels; an absent key is
// n/a. Present reports whether the extra column carried any JSON at all.
type Extra struct {
	RSSI    Null[int]
	Heap    Null[int]
	Temp    Null[int]
	TX      Null[int]
	Present bool
}
