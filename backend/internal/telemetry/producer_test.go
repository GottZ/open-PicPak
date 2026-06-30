package telemetry

import (
	"context"
	"testing"
	"time"
)

// T7 — the telemetry SSE producer carries the per-row liveness the roster channel omits (D22.7). The
// watermark PRIMES silently (no startup flood), then emits exactly one Event per device that pushed
// since, with a server-computed health + last_seen. Red: relying on Doc 19's roster producer alone — it
// excludes last_seen from its diff key (19 §4.5) — never updates per-row liveness/age on the dashboard.
func TestTailTelemetry_WatermarkPollDiff_T7(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)
	ctx := context.Background()
	cfg := DefaultVerdictCfg()

	seedDevice(t, pool, "DEV-A", "stable")
	seedDevice(t, pool, "DEV-B", "stable")
	// a pre-existing row BEFORE priming — must NOT be emitted (the prime swallows the recent window).
	mustExec(t, pool, `INSERT INTO telemetry (time, serial, batt_pct, running_ver) VALUES (now() - interval '1 minute','DEV-A',80,'fw-old')`)

	// cold prime: emits nothing, watermark set to the newest existing row.
	ev, hw, err := repo.TailTelemetry(ctx, Watermark{}, cfg, true, 1000, time.Now())
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("cold prime must emit nothing (no startup flood); got %d", len(ev))
	}
	if !hw.Set {
		t.Fatal("prime must set the watermark")
	}

	// a NEW push for DEV-B after the prime → exactly one Event for DEV-B carrying health + last_seen.
	mustExec(t, pool, `INSERT INTO telemetry (time, serial, batt_pct, running_ver) VALUES (now(),'DEV-B',55,'fw-new')`)
	ev, hw, err = repo.TailTelemetry(ctx, hw, cfg, true, 1000, time.Now())
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(ev) != 1 || ev[0].Serial != "DEV-B" {
		t.Fatalf("want exactly 1 Event for DEV-B; got %+v", ev)
	}
	if ev[0].RunningVer != "fw-new" {
		t.Errorf("running_ver = %q, want fw-new", ev[0].RunningVer)
	}
	if ev[0].Health == "" {
		t.Error("event must carry a server-computed health verdict (one verdict authority)")
	}
	if ev[0].LastSeen == nil {
		t.Error("event must carry last_seen — the per-row liveness the roster diff key omits (T7 crux)")
	}
	if v, ok := ev[0].BattPct.Get(); !ok || v != 55 {
		t.Errorf("batt_pct = (%d,%v), want 55", v, ok)
	}

	// a quiet tick (no new pushes) → zero events, watermark unchanged (sparse → quiet stream).
	ev, _, err = repo.TailTelemetry(ctx, hw, cfg, true, 1000, time.Now())
	if err != nil {
		t.Fatalf("quiet: %v", err)
	}
	if len(ev) != 0 {
		t.Fatalf("a quiet tick must emit nothing; got %d", len(ev))
	}
}
