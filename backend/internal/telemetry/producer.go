package telemetry

import (
	"context"
	"fmt"
	"time"
)

// The SSE telemetry producer's data side (A22 W3 / Design 22 §4.3, D22.7). It runs on Design 19's
// existing hub tick — NOT a NOTIFY listener (ingest does not NOTIFY on a telemetry insert; the only
// trigger is command_queue) and NOT a full LatestFleet re-query every tick. Telemetry is sparse by
// physics (~1 push per battery wake), so a cheap watermark poll matches the arrival pattern and adds no
// failure mode. The DB is the source of truth; the SSE is the accelerator (history rides the REST query).

// Watermark is the forward poll-and-diff cursor the producer advances each hub tick. Set is false until
// the first prime — a cold producer starts from the newest existing row (or DB now() on an empty table)
// and emits ONLY rows that arrive AFTER, so a fresh producer never floods the recent window.
type Watermark struct {
	Time time.Time
	Set  bool
}

// Event is one per-device telemetry delta for the SSE `telemetry` payload (Design 22 §4.3). It fulfills
// Design 19's deferred liveness contract: it carries last_seen + health — the per-row liveness the
// roster producer's diff key deliberately omits. The hub json.Marshal's the whole value (escaping any
// device-supplied running_ver/reset_reason), so a connected operator's stream cannot be frame-forged
// (COH9 / the Doc 21 Q6 integrity property, inherited at hub level).
type Event struct {
	Serial          string     `json:"serial"`
	Time            time.Time  `json:"time"`
	BattPct         Null[int]  `json:"batt_pct"`
	BattMV          Null[int]  `json:"batt_mv"`
	RunningVer      string     `json:"running_ver"`
	Health          string     `json:"health"`
	LastSeen        *time.Time `json:"last_seen"`
	ChannelMismatch bool       `json:"channel_mismatch"`
}

// TailTelemetry advances the watermark and returns one Event per device that pushed since it (D22.7).
//
//   - Cold (hw.Set==false): PRIME — set the watermark to max(time) (or now() on an empty table) and emit
//     NOTHING. So a fresh producer never re-broadcasts the recent window; only future arrivals are sent.
//   - Warm: phase-1 finds the serials that pushed since the watermark — `WHERE time > $wm`, a time
//     predicate, so TimescaleDB chunk-exclusion touches only the newest chunk(s) — and advances the
//     watermark to the new max(time). phase-2 enriches ONLY those serials (the lateral fleet row →
//     server-side Verdict → Event), never the full fleet. A quiet tick → phase-1 zero rows, near-zero cost.
//
// channelMismatchWarn gates the mismatch flag (the same display policy as /api/fleet). now is injected
// for the verdict (a just-pushed device is never OFFLINE_STALE, so it rarely matters — but kept pure).
// The C2-contact clock (c2_last_seen) advances on C2 polls, NOT telemetry inserts, so it is not carried
// here — it is REST/snapshot-sourced (a // TODO(c2-contact-liveness) seam if live C2 deltas are wanted).
func (r *Repo) TailTelemetry(ctx context.Context, hw Watermark, cfg VerdictCfg, channelMismatchWarn bool, maxDevices int, now time.Time) ([]Event, Watermark, error) {
	if !hw.Set {
		var t time.Time
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(max(time), now()) FROM telemetry`).Scan(&t); err != nil {
			return nil, hw, fmt.Errorf("telemetry watermark prime: %w", err)
		}
		return nil, Watermark{Time: t, Set: true}, nil
	}

	// phase-1: the change probe — changed serials + the new high-water in one chunk-excluded query.
	rows, err := r.pool.Query(ctx,
		`SELECT serial, max(time) AS mx FROM telemetry WHERE time > $1 GROUP BY serial`, hw.Time)
	if err != nil {
		return nil, hw, fmt.Errorf("telemetry change probe: %w", err)
	}
	var serials []string
	newHW := hw.Time
	for rows.Next() {
		var s string
		var mx time.Time
		if err := rows.Scan(&s, &mx); err != nil {
			rows.Close()
			return nil, hw, fmt.Errorf("telemetry change scan: %w", err)
		}
		serials = append(serials, s)
		if mx.After(newHW) {
			newHW = mx
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, hw, fmt.Errorf("telemetry change rows: %w", err)
	}
	if len(serials) == 0 {
		return nil, hw, nil // quiet tick — nothing pushed, watermark unchanged
	}

	// phase-2: enrich ONLY the changed serials through the same lateral fleet query → one verdict authority.
	fleet, err := r.FleetForSerials(ctx, serials, maxDevices)
	if err != nil {
		return nil, hw, fmt.Errorf("telemetry enrich: %w", err)
	}
	out := make([]Event, 0, len(fleet))
	for _, row := range fleet {
		if !row.HasData { // a changed serial always has a row; defensive against a racing delete
			continue
		}
		h, _ := Verdict(row.Sample(), cfg, now)
		out = append(out, Event{
			Serial:          row.Serial,
			Time:            row.Time,
			BattPct:         row.BattPct,
			BattMV:          row.BattMV,
			RunningVer:      row.RunningVer,
			Health:          h.String(),
			LastSeen:        row.RegLastSeen,
			ChannelMismatch: channelMismatchWarn && row.ChannelMismatch(),
		})
	}
	return out, Watermark{Time: newHW, Set: true}, nil
}
