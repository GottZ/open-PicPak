package telemetry

import (
	"testing"
	"time"
)

// testVerdictCfg is the default threshold set the verdict tests run against (mirrors
// the [telemetry] defaults, constructed directly so the test needs no config).
func testVerdictCfg() VerdictCfg {
	return VerdictCfg{
		StaleAfter:           3 * time.Hour,
		BrownoutResetReasons: []string{"brownout"},
		BrownoutOTARRCodes:   []int{9},
		BadBootsWarn:         2,
		LowBattPct:           15,
		LowBattIncludesUSB:   false,
	}
}

func hasReason(reasons []string, substr string) bool {
	for _, r := range reasons {
		if len(substr) > 0 && containsSub(r, substr) {
			return true
		}
	}
	return false
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestVerdict_Table(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-1 * time.Minute)
	cfg := testVerdictCfg()

	tests := []struct {
		name string
		s    Sample
		want Health
	}{
		{
			name: "ok",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), BattPct: some(80), BadBoots: some(0), USB: some(false)},
			want: HealthOK,
		},
		{
			name: "brownout via reset_reason",
			s:    Sample{Time: fresh, ResetReason: some("brownout"), BattPct: some(80)},
			want: HealthBrownout,
		},
		{
			name: "brownout via reset_reason mixed case",
			s:    Sample{Time: fresh, ResetReason: some("BROWNOUT")},
			want: HealthBrownout,
		},
		{
			name: "brownout via diag_ota_rr=9",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), DiagOTARR: some(9)},
			want: HealthBrownout,
		},
		{
			name: "diag_ota_rr=5 is NOT brownout (int_wdt)",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), DiagOTARR: some(5)},
			want: HealthOK,
		},
		{
			name: "bad_boots at threshold",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), BadBoots: some(2)},
			want: HealthBadBoots,
		},
		{
			name: "low_batt on battery",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), BattPct: some(10), USB: some(false)},
			want: HealthLowBatt,
		},
		{
			name: "low_batt suppressed on usb",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), BattPct: some(10), USB: some(true)},
			want: HealthOK,
		},
		{
			name: "offline_stale beats brownout (most severe)",
			s:    Sample{Time: now.Add(-4 * time.Hour), ResetReason: some("brownout")},
			want: HealthOffline,
		},
		{
			name: "na batt_pct does not trigger low_batt",
			s:    Sample{Time: fresh, ResetReason: some("poweron"), BattPct: Null[int]{}},
			want: HealthOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Verdict(tc.s, nil, cfg, now)
			if got != tc.want {
				t.Fatalf("Verdict = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestVerdict_LowBattIncludesUSB(t *testing.T) {
	now := time.Now()
	cfg := testVerdictCfg()
	cfg.LowBattIncludesUSB = true
	s := Sample{Time: now, ResetReason: some("poweron"), BattPct: some(5), USB: some(true)}
	if got, _ := Verdict(s, nil, cfg, now); got != HealthLowBatt {
		t.Fatalf("Verdict = %s, want LOW_BATT when low_batt_includes_usb", got)
	}
}

// TestVerdict_SentinelUptimeNotBrownout is the gate's NEGATIVE test: a -1 uptime
// sentinel (Null.Valid==false) must NOT yield BROWNOUT via the uptime-rollback path. We
// feed a non-brownout reset_reason and a sentinel current uptime with a higher (valid)
// previous uptime; the row is OK, not BROWNOUT.
func TestVerdict_SentinelUptimeNotBrownout(t *testing.T) {
	now := time.Now()
	cfg := testVerdictCfg()
	cur := Sample{
		Time:        now,
		ResetReason: some("panic"), // NOT a brownout reason
		Uptime:      Null[int64]{}, // -1 sentinel mapped to n/a at scan
	}
	prev := &Sample{Time: now.Add(-time.Hour), Uptime: some(int64(500000))}
	got, reasons := Verdict(cur, prev, cfg, now)
	if got == HealthBrownout {
		t.Fatalf("sentinel uptime falsely yielded BROWNOUT (reasons=%v)", reasons)
	}
	if got != HealthOK {
		t.Fatalf("Verdict = %s, want OK", got)
	}
}

// TestUptimeRollback_SkipsSentinel proves the sentinel skip in isolation: a sentinel
// (invalid) uptime is never read as a drop, even against a valid higher previous.
func TestUptimeRollback_SkipsSentinel(t *testing.T) {
	prev := &Sample{Uptime: some(int64(900000))}
	sentinel := Sample{Uptime: Null[int64]{}} // -1/NULL → invalid
	if UptimeRollback(sentinel, prev) {
		t.Fatal("sentinel uptime must not be read as a rollback")
	}
	// A genuine valid drop IS a rollback.
	drop := Sample{Uptime: some(int64(1000))}
	if !UptimeRollback(drop, prev) {
		t.Fatal("a valid uptime drop should be detected as a rollback")
	}
	// No previous → never a rollback.
	if UptimeRollback(drop, nil) {
		t.Fatal("no previous sample must not be a rollback")
	}
}

// TestVerdict_BrownoutReasonNotFromSentinel proves that when reset_reason IS brownout
// but uptime is the sentinel, the BROWNOUT verdict comes from the reset_reason path,
// NOT the (skipped) rollback path — the reasons list must not claim a rollback.
func TestVerdict_BrownoutReasonNotFromSentinel(t *testing.T) {
	now := time.Now()
	cfg := testVerdictCfg()
	cur := Sample{Time: now, ResetReason: some("brownout"), Uptime: Null[int64]{}}
	prev := &Sample{Uptime: some(int64(800000))}
	got, reasons := Verdict(cur, prev, cfg, now)
	if got != HealthBrownout {
		t.Fatalf("Verdict = %s, want BROWNOUT (via reset_reason)", got)
	}
	if !hasReason(reasons, "reset_reason=brownout") {
		t.Fatalf("expected a reset_reason brownout reason, got %v", reasons)
	}
	if hasReason(reasons, "rollback") {
		t.Fatalf("sentinel uptime must not produce a rollback reason, got %v", reasons)
	}
}

// TestVerdict_GenuineRollback: a real uptime drop with a brownout reset records both
// the reset and the rollback reason.
func TestVerdict_GenuineRollback(t *testing.T) {
	now := time.Now()
	cfg := testVerdictCfg()
	cur := Sample{Time: now, ResetReason: some("brownout"), Uptime: some(int64(1200))}
	prev := &Sample{Uptime: some(int64(990000))}
	got, reasons := Verdict(cur, prev, cfg, now)
	if got != HealthBrownout {
		t.Fatalf("Verdict = %s, want BROWNOUT", got)
	}
	if !hasReason(reasons, "rollback") {
		t.Fatalf("expected a rollback reason, got %v", reasons)
	}
}
