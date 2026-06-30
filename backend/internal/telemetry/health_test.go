package telemetry

import (
	"testing"
	"time"
)

// Pure, single-row verdict properties — no DB (D22.4). The verdict is table-tested from crafted Samples;
// a verdict computed in SQL, or one needing a cross-row uptime[n-1], could not be tested this way.

var now = time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

func cfg() VerdictCfg { return DefaultVerdictCfg() }

func ptr[T any](v T) *T { return &v }

// T4 — Verdict table-test: each Health from a single crafted row, including NO_DATA's two reasons.
func TestVerdict_Table_T4(t *testing.T) {
	fresh := now.Add(-1 * time.Minute) // within stale_after

	cases := []struct {
		name       string
		s          Sample
		wantHealth Health
		wantReason string // a substring that must appear in some reason (empty = skip the check)
	}{
		{
			name:       "OK — fresh, nothing triggered",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(90), USB: some(false)},
			wantHealth: HealthOK,
		},
		{
			name:       "LOW_BATT — at threshold, not USB",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(15), USB: some(false)},
			wantHealth: HealthLowBatt,
			wantReason: "batt_pct=15",
		},
		{
			name:       "LOW_BATT suppressed on USB",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(10), USB: some(true)},
			wantHealth: HealthOK,
		},
		{
			name:       "BAD_BOOTS — guard climbing",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(90), USB: some(false), BadBoots: some(2)},
			wantHealth: HealthBadBoots,
			wantReason: "bad_boots=2",
		},
		{
			name:       "BROWNOUT — reset_reason wire string",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(90), USB: some(false), ResetReason: some("brownout")},
			wantHealth: HealthBrownout,
			wantReason: "reset_reason=brownout",
		},
		{
			name:       "BROWNOUT — diag_ota_rr code 9",
			s:          Sample{HasData: true, Time: fresh, BattPct: some(90), USB: some(false), DiagOTARR: some(9)},
			wantHealth: HealthBrownout,
			wantReason: "diag_ota_rr=9",
		},
		{
			name:       "OFFLINE_STALE — beyond stale_after, most-severe over LOW_BATT",
			s:          Sample{HasData: true, Time: now.Add(-4 * time.Hour), BattPct: some(5), USB: some(false)},
			wantHealth: HealthOffline,
		},
		{
			name:       "NO_DATA c2_alive — no telemetry, fresh C2 contact",
			s:          Sample{HasData: false, C2LastSeen: ptr(now.Add(-10 * time.Minute))},
			wantHealth: HealthNoData,
			wantReason: ReasonC2Alive,
		},
		{
			name:       "NO_DATA silent — no telemetry, stale C2 contact",
			s:          Sample{HasData: false, C2LastSeen: ptr(now.Add(-9 * time.Hour))},
			wantHealth: HealthNoData,
			wantReason: ReasonSilent,
		},
		{
			name:       "NO_DATA silent — no telemetry, never any C2 contact",
			s:          Sample{HasData: false, C2LastSeen: nil},
			wantHealth: HealthNoData,
			wantReason: ReasonSilent,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, reasons := Verdict(c.s, cfg(), now)
			if h != c.wantHealth {
				t.Fatalf("health = %s, want %s (reasons=%v)", h, c.wantHealth, reasons)
			}
			if c.wantReason != "" && !anyContains(reasons, c.wantReason) {
				t.Fatalf("reasons %v do not contain %q", reasons, c.wantReason)
			}
		})
	}
}

// T3 — the single-row verdict NEVER sets BROWNOUT from a bare low/dropped uptime (it does not read
// uptime at all), and the cross-row RollbackChip skips a sentinel/NULL uptime so a "not measured" value
// is never read as a drop → false brownout (D22.4/D22.5).
func TestVerdict_NoUptimeBrownout_T3(t *testing.T) {
	fresh := now.Add(-time.Minute)

	// (a) lone single row with a low uptime but a NON-brownout reset → OK, never BROWNOUT.
	h, _ := Verdict(Sample{HasData: true, Time: fresh, BattPct: some(90), USB: some(false), Uptime: some(int64(5)), ResetReason: some("poweron")}, cfg(), now)
	if h != HealthOK {
		t.Fatalf("lone low uptime must not raise BROWNOUT; got %s", h)
	}

	// (b) RollbackChip: a true drop with a brownout reset → chip fires.
	cur := Sample{Uptime: some(int64(10)), ResetReason: some("brownout")}
	prev := Sample{Uptime: some(int64(900))}
	if !RollbackChip(cur, prev, cfg()) {
		t.Fatal("a true uptime drop + brownout reset must fire the rollback chip")
	}

	// (c) sentinel poison: the newer uptime is the -1 sentinel (Null invalid after scan) → NO chip,
	// even with a brownout reset (a sentinel must never read as an uptime drop).
	curSentinel := Sample{Uptime: Null[int64]{}, ResetReason: some("brownout")}
	if RollbackChip(curSentinel, prev, cfg()) {
		t.Fatal("a sentinel/NULL uptime must not read as a rollback (false-brownout poison)")
	}

	// (d) no brownout reset → no chip even on a real drop (the AND qualifier).
	if RollbackChip(Sample{Uptime: some(int64(10)), ResetReason: some("poweron")}, prev, cfg()) {
		t.Fatal("a benign reboot (drop without brownout reset) must not fire the chip")
	}
}

func anyContains(reasons []string, sub string) bool {
	for _, r := range reasons {
		if contains(r, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
