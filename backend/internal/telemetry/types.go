// Package telemetry is the operator-facing READ side of the device telemetry axis (A22 / Design 22,
// the M5 operator zone). It SELECTs from the `telemetry` hypertable plus `devices`/`device_auth` and
// presents a per-device health view; it WRITES NOTHING (D22.1) and is imported by cmd/admin only — the
// public ingest parser keeps its own write INSERT and never links this package (a structural-hygiene
// guard mirrored from Doc 17 T6 / Doc 19 T7; telemetry carries no secret, so the guard is hygiene, not
// the secret boundary itself).
//
// Lifted + adapted from the TUI read core (design-tui/08-telemetry.md §3) minus the Bubble Tea files.
// The v0.2 deltas this package carries over the lift:
//   - the lateral-over-`devices` fleet query so a registered-but-silent device survives (D22.3) — every
//     device is silent today, the field-path push is unwired (§2);
//   - a pure SINGLE-ROW Verdict (D22.4): the uptime-rollback brownout signal is a cross-row
//     corroboration computed only over history, never in the single-row verdict;
//   - the NO_DATA split by the C2-contact clock (D22.12): a device polling C2 with telemetry unwired
//     (the current normal) vs a truly silent one.
//
// This file holds the value types: the Null[T] optional, the Health verdict enum, the per-row structs
// the repository scans into, and the per-column sentinel constants.
package telemetry

import (
	"encoding/json"
	"time"
)

// Null[T] is a typed optional. Valid is false for a SQL NULL or a per-column sentinel (mapped at scan
// time in sentinel.go); a consumer renders an invalid Null as "n/a" and never reads V. This keeps "not
// measured this boot" (firmware 02-telemetry §3.5) out of the trend/verdict math. It marshals to the
// bare value when present and to JSON null when absent — so the read structs serialize straight to the
// admin API shape with no per-field DTO (W2/W3).
type Null[T any] struct {
	V     T
	Valid bool
}

// Get returns the value and whether it is present.
func (n Null[T]) Get() (T, bool) { return n.V, n.Valid }

// MarshalJSON emits the value when present, JSON null when absent (the "n/a" wire form).
func (n Null[T]) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(n.V)
}

// some wraps a present value.
func some[T any](v T) Null[T] { return Null[T]{V: v, Valid: true} }

// Per-column sentinels (design 08 §5; firmware 02-telemetry §3.2/§3.5). These are assigned PER METRIC by
// the firmware, so the reader maps column → its own sentinel set, never a blanket list applied to every
// integer column. Critically -127/-32768 are bound ONLY to the JSONB rssi/temp metrics (which have no
// column), so a genuine -32768 in a SMALLINT diag_* column (range -32768..32767) is never masked.
const (
	// SentinelGeneric is the "not measured this boot" value for the generic numeric columns uptime_ms,
	// batt_mv, batt_pct (plus SQL NULL → n/a).
	SentinelGeneric int64 = -1
	// SentinelRSSI is the rssi sentinel — extra JSONB only (no column).
	SentinelRSSI int = -127
	// SentinelTemp is the temp sentinel — extra JSONB only (no column).
	SentinelTemp int = -32768
)

// Health is the per-device verdict (design 08 §2). Higher values are MORE severe; Verdict returns the
// most severe applicable verdict (OK when none apply). NoData is its own state — distinct from
// OFFLINE_STALE (which had data, now stale) — reached only when no telemetry row exists at all (D22.4);
// it is never produced by the severity bump, so its high ordinal never interferes with the data path.
type Health int

const (
	HealthOK       Health = iota // no condition triggered
	HealthLowBatt                // batt_pct <= low_batt_pct and (not USB | includes_usb)
	HealthBadBoots               // bad_boots >= bad_boots_warn (guard climbing to safe-mode)
	HealthBrownout               // brownout reset_reason / diag_ota_rr code (single-row only)
	HealthOffline                // now-time > stale_after (offline OR dead — unresolvable here)
	HealthNoData                 // no telemetry row at all (D22.4); split c2_alive/silent by D22.12
)

// String renders the canonical verdict label used on the wire, in the UI and in tests.
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
	case HealthNoData:
		return "NO_DATA"
	default:
		return "UNKNOWN"
	}
}

// NO_DATA reason tokens (D22.12). They are stable machine tokens (not human prose) so the verdict stays
// table-testable and the SPA owns the friendly rendering ("C2-alive, awaiting telemetry" vs "silent — no
// C2 contact"). The data verdicts carry human reason strings; only the NO_DATA branch uses these tokens.
const (
	ReasonC2Alive = "c2_alive" // no telemetry, but fresh C2 contact (the §2 current normal)
	ReasonSilent  = "silent"   // no telemetry AND no/stale C2 contact (truly dark)
)

// Sample is the minimal field set the single-row verdict needs from one read row. HasData is false when
// no telemetry row exists (the NO_DATA path); C2LastSeen is the device_auth liveness clock that splits
// NO_DATA into c2_alive vs silent (D22.12). FleetRow/DeviceTelemetry/HistoryPoint each expose Sample(),
// so the verdict is computed identically from a fleet snapshot row or a detail row with no DB in health.go.
type Sample struct {
	HasData     bool
	Time        time.Time
	ResetReason Null[string]
	DiagOTARR   Null[int] // diag_ota_rr (SMALLINT, NULL-only sentinel)
	BadBoots    Null[int]
	BattPct     Null[int]
	USB         Null[bool]
	Uptime      Null[int64]  // uptime_ms (-1 + NULL sentinel applied at scan)
	C2LastSeen  *time.Time   // device_auth.last_seen_at — liveness clock for the NO_DATA split (D22.12)
}

// FleetRow is one device's enriched fleet-list row (D22.2/D22.3): the control-plane identity/membership
// (devices + device_auth) LEFT JOIN LATERAL its newest telemetry row. HasData is false when the lateral
// join missed (no telemetry yet — every device today, §2); the telemetry fields are then zero/invalid
// and the verdict is NO_DATA. AssignedChannel is the AUTHORITATIVE devices.channel; ReportedChannel is
// the device-claimed telemetry.channel (D22.6). RegLastSeen (devices.last_seen, the data clock) and
// C2LastSeen (device_auth.last_seen_at, the liveness clock) are the two distinct freshness clocks (D22.12).
type FleetRow struct {
	Serial          string     `json:"serial"`
	Label           *string    `json:"label"`
	AssignedChannel string     `json:"assigned_channel"`
	RegLastSeen     *time.Time `json:"reg_last_seen"`
	Bonded          bool       `json:"bonded"`
	C2LastSeen      *time.Time `json:"c2_last_seen"`

	HasData         bool        `json:"has_data"`
	Time            time.Time   `json:"time"`
	BattMV          Null[int]   `json:"batt_mv"`
	BattPct         Null[int]   `json:"batt_pct"`
	BadBoots        Null[int]   `json:"bad_boots"`
	BootCount       Null[int64] `json:"boot_count"`
	ResetReason     Null[string] `json:"reset_reason"`
	USB             Null[bool]  `json:"usb"`
	Uptime          Null[int64] `json:"uptime_ms"`
	RunningVer      string      `json:"running_ver"`
	ReportedChannel string      `json:"reported_channel"`
	DiagOTARR       Null[int]   `json:"diag_ota_rr"`
}

// Sample projects the verdict-relevant fields (incl. the liveness clock for the NO_DATA split).
func (r FleetRow) Sample() Sample {
	return Sample{
		HasData:     r.HasData,
		Time:        r.Time,
		ResetReason: r.ResetReason,
		DiagOTARR:   r.DiagOTARR,
		BadBoots:    r.BadBoots,
		BattPct:     r.BattPct,
		USB:         r.USB,
		Uptime:      r.Uptime,
		C2LastSeen:  r.C2LastSeen,
	}
}

// ChannelMismatch reports a device claiming a channel it is not assigned (D22.6) — meaningful only when
// a telemetry row exists AND it carried a channel. The authoritative answer is always AssignedChannel;
// a mismatch must not mislead a rollout read (Doc 20).
func (r FleetRow) ChannelMismatch() bool {
	return r.HasData && r.ReportedChannel != "" && r.ReportedChannel != r.AssignedChannel
}

// DeviceTelemetry is the latest FULL row for the selected device (read-shape 2a), including the diag_*
// OTA fields and the opportunistically-decoded extra JSONB metrics (rssi/heap/temp/tx). rssi/temp live
// ONLY in extra (no column), so they are read here, not in the fleet snapshot.
type DeviceTelemetry struct {
	Serial        string       `json:"serial"`
	Time          time.Time    `json:"time"`
	BattMV        Null[int]    `json:"batt_mv"`
	BattPct       Null[int]    `json:"batt_pct"`
	BadBoots      Null[int]    `json:"bad_boots"`
	BootCount     Null[int64]  `json:"boot_count"`
	ResetReason   Null[string] `json:"reset_reason"`
	USB           Null[bool]   `json:"usb"`
	Uptime        Null[int64]  `json:"uptime_ms"`
	RunningVer    string       `json:"running_ver"`
	DeviceChannel string       `json:"device_channel"`

	DiagRunPart  Null[string] `json:"diag_run_part"`
	DiagRunstate Null[int]    `json:"diag_runstate"` // SMALLINT, NULL-only sentinel (real -32768 survives)
	DiagO0       Null[int]    `json:"diag_o0"`       // SMALLINT, NULL-only sentinel
	DiagO1       Null[int]    `json:"diag_o1"`       // SMALLINT, NULL-only sentinel
	DiagInv      Null[string] `json:"diag_inv"`
	DiagBoots    Null[int64]  `json:"diag_boots"`
	DiagRR       Null[int]    `json:"diag_rr"`
	DiagOTARR    Null[int]    `json:"diag_ota_rr"`
	DiagMV       Null[int64]  `json:"diag_mv"`
	DiagMVErr    Null[int]    `json:"diag_mv_err"`
	DiagStage    Null[string] `json:"diag_stage"`

	Extra Extra `json:"extra"` // decoded extra JSONB (rssi/heap/temp/tx), all n/a when absent
}

// Sample projects the verdict-relevant fields. A detail row always HasData (it was scanned from a row).
func (d DeviceTelemetry) Sample() Sample {
	return Sample{
		HasData:     true,
		Time:        d.Time,
		ResetReason: d.ResetReason,
		DiagOTARR:   d.DiagOTARR,
		BadBoots:    d.BadBoots,
		BattPct:     d.BattPct,
		USB:         d.USB,
		Uptime:      d.Uptime,
	}
}

// HistoryPoint is one row of the per-device history window, the series behind the trend sparklines and
// the recent-history table (read-shape 2b). ResetReason + Uptime are kept so the cross-row uptime-rollback
// brownout corroboration can walk consecutive rows (RollbackChip); DiagOTARR lets the card chip a
// brownout-during-OTA per point.
type HistoryPoint struct {
	Time        time.Time    `json:"time"`
	BattMV      Null[int]    `json:"batt_mv"`
	BattPct     Null[int]    `json:"batt_pct"`
	BadBoots    Null[int]    `json:"bad_boots"`
	BootCount   Null[int64]  `json:"boot_count"`
	ResetReason Null[string] `json:"reset_reason"`
	Uptime      Null[int64]  `json:"uptime_ms"`
	DiagOTARR   Null[int]    `json:"diag_ota_rr"`
}

// Sample projects the verdict-relevant fields. HasData is true (it was scanned from a row).
func (h HistoryPoint) Sample() Sample {
	return Sample{
		HasData:     true,
		Time:        h.Time,
		ResetReason: h.ResetReason,
		DiagOTARR:   h.DiagOTARR,
		BadBoots:    h.BadBoots,
		Uptime:      h.Uptime,
	}
}

// Extra is the opportunistically-decoded telemetry.extra JSONB (design 08 §1.1): rssi/heap/temp/tx live
// ONLY here (no column) so a future ingest can grow metrics without a schema migration. rssi/temp apply
// their own sentinels; an absent key is n/a. Present reports whether the extra column carried any JSON.
type Extra struct {
	RSSI    Null[int] `json:"rssi"`
	Heap    Null[int] `json:"heap"`
	Temp    Null[int] `json:"temp"`
	TX      Null[int] `json:"tx"`
	Present bool      `json:"present"`
}
