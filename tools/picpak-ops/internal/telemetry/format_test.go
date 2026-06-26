package telemetry

import (
	"testing"
	"time"
)

func TestBattString(t *testing.T) {
	if got := BattString(some(4180), "V"); got != "4.18 V" {
		t.Fatalf("BattString V = %q", got)
	}
	if got := BattString(some(4180), "mV"); got != "4180 mV" {
		t.Fatalf("BattString mV = %q", got)
	}
	if got := BattString(Null[int]{}, "V"); got != naLabel {
		t.Fatalf("BattString n/a = %q", got)
	}
}

func TestUptimeString(t *testing.T) {
	if got := UptimeString(Null[int64]{}); got != naLabel {
		t.Fatalf("sentinel uptime should render n/a, got %q", got)
	}
	if got := UptimeString(some(int64(123456))); got != "2m3s" {
		t.Fatalf("UptimeString = %q, want 2m3s", got)
	}
}

func TestAge(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "never"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
		{now.Add(time.Hour), "just now"}, // future clamps
	}
	for _, c := range cases {
		if got := Age(c.t, now); got != c.want {
			t.Errorf("Age(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestResetReasonString(t *testing.T) {
	if got := ResetReasonString(some("brownout")); got != "brownout" {
		t.Fatalf("brownout = %q", got)
	}
	if got := ResetReasonString(some("int_wdt")); got != "interrupt WDT" {
		t.Fatalf("int_wdt = %q", got)
	}
	if got := ResetReasonString(some("future_reason")); got != "future_reason" {
		t.Fatalf("unknown reason should render verbatim, got %q", got)
	}
	if got := ResetReasonString(Null[string]{}); got != naLabel {
		t.Fatalf("n/a = %q", got)
	}
}

func TestChannelMismatch(t *testing.T) {
	if got := ChannelMismatch("beta", "stable", true); got == "" {
		t.Fatal("a real mismatch should produce a badge")
	}
	if got := ChannelMismatch("stable", "stable", true); got != "" {
		t.Fatalf("equal channels should not flag, got %q", got)
	}
	if got := ChannelMismatch("beta", "stable", false); got != "" {
		t.Fatalf("warn=false should not flag, got %q", got)
	}
	if got := ChannelMismatch("", "stable", true); got != "" {
		t.Fatalf("empty claimed should not flag, got %q", got)
	}
}

func TestSparkline(t *testing.T) {
	// All n/a → naLabel, not a row of zeros.
	if got := sparkline([]Null[int]{{}, {}}); got != naLabel {
		t.Fatalf("all-n/a sparkline = %q, want n/a", got)
	}
	// A flat series → a row of the lowest bar (no division by zero).
	got := sparkline([]Null[int]{some(5), some(5), some(5)})
	if len([]rune(got)) != 3 {
		t.Fatalf("flat sparkline width = %d, want 3", len([]rune(got)))
	}
	// A gap (n/a) renders as a space, not a plotted point.
	g := sparkline([]Null[int]{some(1), {}, some(8)})
	if []rune(g)[1] != ' ' {
		t.Fatalf("n/a point should be a gap, got %q", string([]rune(g)[1]))
	}
}
