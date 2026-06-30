package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB-backed read properties (skipped unless TEST_DATABASE_URL is set; run in the e2e gate). Each test
// inserts telemetry/devices DIRECTLY (this is the read side, not ingest) and states its red. The harness
// mirrors internal/logquery's: one shared pool, FK-safe reset at the top of each test.

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, stmt := range []string{
		`TRUNCATE telemetry`,
		`DELETE FROM devices`, // cascades device_auth (+cursor/fragment) via FK
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedDevice inserts a registered device with the given authoritative channel (empty → 'stable').
func seedDevice(t *testing.T, pool *pgxpool.Pool, serial, channel string) {
	t.Helper()
	if channel == "" {
		channel = "stable"
	}
	mustExec(t, pool, `INSERT INTO devices (serial, channel, last_seen) VALUES ($1,$2,now())`, serial, channel)
}

// seedAuth inserts a device_auth row (the bond) with a controllable C2-contact clock. After the C2
// cutover (0005) the table is keyed by ecdsa_pubkey, not the dropped hotp_secret; the read path needs
// only session_bootstrapped (→ bonded) and last_seen_at (→ the c2_last_seen liveness clock), so the
// pubkey can stay NULL.
func seedAuth(t *testing.T, pool *pgxpool.Pool, serial string, lastSeenAt *time.Time, bootstrapped bool) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO device_auth (serial, last_seen_at, session_bootstrapped) VALUES ($1,$2,$3)`,
		serial, lastSeenAt, bootstrapped)
}

func fleetBySerial(rows []FleetRow) map[string]FleetRow {
	m := map[string]FleetRow{}
	for _, r := range rows {
		m[r.Serial] = r
	}
	return m
}

// T1 — a registered device with ZERO telemetry survives the fleet list as NO_DATA. RED: a
// `DISTINCT ON (serial) FROM telemetry` query drops it — and that is EVERY device today (the empty-table
// normal, §2), so the dashboard would show an empty fleet. The lateral-over-devices query is the fix (D22.3).
func TestLatestFleet_SilentDeviceSurvives_T1(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)
	ctx := context.Background()

	seedDevice(t, pool, "DEV-SILENT", "stable")             // never pushed telemetry
	seedDevice(t, pool, "DEV-LIVE", "stable")               // has a telemetry row
	mustExec(t, pool,
		`INSERT INTO telemetry (time, serial, batt_pct, running_ver, channel) VALUES (now(),$1,77,'fw-1.2.3','stable')`,
		"DEV-LIVE")

	rows, err := repo.LatestFleet(ctx, 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	by := fleetBySerial(rows)
	if len(by) != 2 {
		t.Fatalf("expected BOTH registered devices in the fleet, got %d: %v", len(by), keys(by))
	}

	silent := by["DEV-SILENT"]
	if silent.HasData {
		t.Fatal("silent device must have HasData=false")
	}
	if h, _ := Verdict(silent.Sample(), DefaultVerdictCfg(), time.Now()); h != HealthNoData {
		t.Fatalf("silent device verdict = %s, want NO_DATA", h)
	}

	live := by["DEV-LIVE"]
	if !live.HasData {
		t.Fatal("device with telemetry must have HasData=true")
	}
	if live.RunningVer != "fw-1.2.3" {
		t.Fatalf("running_ver = %q, want fw-1.2.3", live.RunningVer)
	}
	if v, ok := live.BattPct.Get(); !ok || v != 77 {
		t.Fatalf("batt_pct = (%d,%v), want 77", v, ok)
	}
}

// T6 — the device-claimed channel is flagged on mismatch; the authoritative channel is always
// devices.channel. RED: trusting telemetry.channel as authoritative misleads a rollout read (Doc 20).
func TestLatestFleet_ChannelMismatch_T6(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)

	seedDevice(t, pool, "DEV-MISMATCH", "stable") // authoritative = stable
	mustExec(t, pool,
		`INSERT INTO telemetry (time, serial, channel, running_ver) VALUES (now(),$1,'beta','fw-x')`, "DEV-MISMATCH")
	seedDevice(t, pool, "DEV-MATCH", "stable")
	mustExec(t, pool,
		`INSERT INTO telemetry (time, serial, channel, running_ver) VALUES (now(),$1,'stable','fw-x')`, "DEV-MATCH")

	rows, err := repo.LatestFleet(context.Background(), 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	by := fleetBySerial(rows)

	mm := by["DEV-MISMATCH"]
	if mm.AssignedChannel != "stable" || mm.ReportedChannel != "beta" || !mm.ChannelMismatch() {
		t.Fatalf("mismatch row: assigned=%q reported=%q mismatch=%v, want stable/beta/true",
			mm.AssignedChannel, mm.ReportedChannel, mm.ChannelMismatch())
	}
	if by["DEV-MATCH"].ChannelMismatch() {
		t.Fatal("a matching channel must not flag a mismatch")
	}
}

// LatestDevice — sparse (unknown serial → nil,nil, never an error) and a full row whose per-column
// sentinels resolve correctly end-to-end (a real -32768 in a SMALLINT diag column survives; the temp
// JSONB sentinel is n/a). This is the scan-path proof of D22.5/D22.9.
func TestLatestDevice_SparseAndFull(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)
	ctx := context.Background()

	none, err := repo.LatestDevice(ctx, "no-such-serial")
	if err != nil {
		t.Fatalf("unknown serial must not error: %v", err)
	}
	if none != nil {
		t.Fatal("unknown serial must yield nil detail (sparse tolerance)")
	}

	seedDevice(t, pool, "DEV-FULL", "stable")
	mustExec(t, pool,
		`INSERT INTO telemetry (time, serial, batt_mv, batt_pct, running_ver, channel, diag_runstate, diag_ota_rr, extra)
		 VALUES (now(),$1,3990,64,'fw-9','stable',-32768,9,'{"rssi":-55,"temp":-32768,"heap":40000}'::jsonb)`,
		"DEV-FULL")

	d, err := repo.LatestDevice(ctx, "DEV-FULL")
	if err != nil || d == nil {
		t.Fatalf("LatestDevice(DEV-FULL) = (%v,%v)", d, err)
	}
	if v, ok := d.DiagRunstate.Get(); !ok || v != -32768 {
		t.Fatalf("diag_runstate -32768 must survive as a real SMALLINT; got (%d,%v)", v, ok)
	}
	if !d.Extra.Present {
		t.Fatal("extra must be Present")
	}
	if v, ok := d.Extra.RSSI.Get(); !ok || v != -55 {
		t.Fatalf("rssi = (%d,%v), want -55", v, ok)
	}
	if _, ok := d.Extra.Temp.Get(); ok {
		t.Fatal("temp -32768 JSONB sentinel must be n/a (NOT the SMALLINT survival case)")
	}
	if v, ok := d.Extra.Heap.Get(); !ok || v != 40000 {
		t.Fatalf("heap = (%d,%v), want 40000", v, ok)
	}
}

// T9 — DeviceHistory is DOUBLE bounded: the row cap AND the time window. RED: an unbounded history OOMs
// the response on a chatty/cabled device (D22.8).
func TestDeviceHistory_DoubleBounded_T9(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)
	ctx := context.Background()

	seedDevice(t, pool, "DEV-HIST", "stable")
	// five in-window rows (minutes apart) + one OUTSIDE the 72h window.
	for i := 0; i < 5; i++ {
		mustExec(t, pool,
			`INSERT INTO telemetry (time, serial, batt_pct) VALUES (now() - make_interval(mins => $2),$1,$3)`,
			"DEV-HIST", i, 50+i)
	}
	mustExec(t, pool,
		`INSERT INTO telemetry (time, serial, batt_pct) VALUES (now() - interval '100 hours',$1,1)`, "DEV-HIST")

	// cap below the in-window count → exactly the cap, newest-first.
	hist, err := repo.DeviceHistory(ctx, "DEV-HIST", 72*time.Hour, 3)
	if err != nil {
		t.Fatalf("DeviceHistory: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("row cap not honored: got %d, want 3", len(hist))
	}
	for i := 1; i < len(hist); i++ {
		if hist[i].Time.After(hist[i-1].Time) {
			t.Fatal("history must be ordered time DESC")
		}
	}
	// a generous cap still excludes the out-of-window row (5 in-window, NOT 6).
	all, err := repo.DeviceHistory(ctx, "DEV-HIST", 72*time.Hour, 500)
	if err != nil {
		t.Fatalf("DeviceHistory(all): %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("time window not honored: got %d, want 5 (the 100h-old row excluded)", len(all))
	}
}

// T12 — NO_DATA splits c2_alive from silent by the device_auth.last_seen_at clock, end-to-end through the
// real LatestFleet join + the verdict. RED: collapsing both to a bare NO_DATA makes a device polling C2
// fine (telemetry just unwired, §2) read as dead — the current normal reads as a dead fleet (D22.12).
func TestLatestFleet_NoDataSplit_T12(t *testing.T) {
	pool := dbPool(t)
	repo := NewRepo(pool)
	ctx := context.Background()
	nowT := time.Now()
	cfg := DefaultVerdictCfg() // stale_after 3h

	fresh := nowT.Add(-10 * time.Minute)
	stale := nowT.Add(-9 * time.Hour)

	seedDevice(t, pool, "DEV-C2ALIVE", "stable")
	seedAuth(t, pool, "DEV-C2ALIVE", &fresh, true) // polls C2, no telemetry
	seedDevice(t, pool, "DEV-SILENT", "stable")
	seedAuth(t, pool, "DEV-SILENT", &stale, true) // stale C2 contact, no telemetry
	seedDevice(t, pool, "DEV-NOAUTH", "stable")   // no device_auth row at all → c2_last_seen NULL

	rows, err := repo.LatestFleet(ctx, 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	by := fleetBySerial(rows)

	check := func(serial, wantReason string) {
		r := by[serial]
		if r.HasData {
			t.Fatalf("%s must have no telemetry", serial)
		}
		h, reasons := Verdict(r.Sample(), cfg, nowT)
		if h != HealthNoData {
			t.Fatalf("%s verdict = %s, want NO_DATA", serial, h)
		}
		if len(reasons) == 0 || reasons[0] != wantReason {
			t.Fatalf("%s reasons = %v, want [%s]", serial, reasons, wantReason)
		}
	}
	check("DEV-C2ALIVE", ReasonC2Alive)
	check("DEV-SILENT", ReasonSilent)
	check("DEV-NOAUTH", ReasonSilent)

	// the c2-alive device must carry the bond + the fresh liveness clock for the footer (D22.12).
	if a := by["DEV-C2ALIVE"]; !a.Bonded || a.C2LastSeen == nil || a.C2LastSeen.Before(fresh.Add(-time.Second)) {
		t.Fatalf("c2-alive row must carry bonded + the fresh c2_last_seen; got bonded=%v c2=%v", a.Bonded, a.C2LastSeen)
	}
}

func keys(m map[string]FleetRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
