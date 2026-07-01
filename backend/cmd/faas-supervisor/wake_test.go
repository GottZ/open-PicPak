package main

import (
	"testing"
	"time"
)

func TestNextWakeSeconds(t *testing.T) {
	c := WakeConfig{NightStartHour: 23, NightEndHour: 6, DayInterval: 3600, MaxWake: 8 * 3600}

	day := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if got := c.NextWakeSeconds(day); got != 3600 {
		t.Errorf("daytime: got %d, want 3600 (DayInterval)", got)
	}
	// 02:00 -> sleep to 06:00 (4h) + morningBuffer(120) = 14520
	night := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	if got := c.NextWakeSeconds(night); got != 14520 {
		t.Errorf("night 02:00: got %d, want 14520", got)
	}
	// 23:30 -> sleep to tomorrow 06:00 (6h30m) + 120 = 23520
	late := time.Date(2026, 7, 1, 23, 30, 0, 0, time.UTC)
	if got := c.NextWakeSeconds(late); got != 23520 {
		t.Errorf("night 23:30: got %d, want 23520", got)
	}
}

// T14 — a function's next_wake_hint is clamped to [60, MaxWake]; it can extend/shorten within bounds
// but never sleep a device for days or hot-loop it.
func TestClampHint(t *testing.T) {
	c := WakeConfig{MaxWake: 8 * 3600}
	if got := c.ClampHint(5); got != 60 {
		t.Errorf("hint 5: got %d, want 60 (floor)", got)
	}
	if got := c.ClampHint(999999); got != 8*3600 {
		t.Errorf("hint 999999: got %d, want %d (ceiling)", got, 8*3600)
	}
	if got := c.ClampHint(3600); got != 3600 {
		t.Errorf("hint 3600: got %d, want 3600 (in-bounds passes)", got)
	}
}
