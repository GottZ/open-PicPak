package main

import "time"

// WakeConfig is the day/night render cadence (a port of server/wake.py's config values). It is
// Policy=Data: the values come from a function's stored cadence or the supervisor env defaults,
// never a code constant.
type WakeConfig struct {
	NightStartHour int
	NightEndHour   int
	DayInterval    int // seconds, daytime cadence
	MaxWake        int // seconds, hard ceiling
}

// morningBuffer wakes a device shortly after NIGHT_END so the first daytime render has fresh data
// (server/wake.py _MORNING_BUFFER).
const morningBuffer = 120

func (c WakeConfig) isNight(hour int) bool {
	s, e := c.NightStartHour, c.NightEndHour
	if s == e {
		return false
	}
	if s < e {
		return s <= hour && hour < e
	}
	return hour >= s || hour < e // wraps midnight (e.g. 23..6)
}

// NextWakeSeconds ports next_wake_seconds (wake.py:24-32): daytime → DayInterval; night → sleep to
// NIGHT_END + morningBuffer; clamped to [60, MaxWake].
func (c WakeConfig) NextWakeSeconds(now time.Time) int {
	var secs int
	if c.isNight(now.Hour()) {
		target := time.Date(now.Year(), now.Month(), now.Day(), c.NightEndHour, 0, 0, 0, now.Location())
		if now.Hour() >= c.NightEndHour { // still before midnight → target is tomorrow
			target = target.AddDate(0, 0, 1)
		}
		secs = int(target.Sub(now).Seconds()) + morningBuffer
	} else {
		secs = c.DayInterval
	}
	return clampWake(secs, c.MaxWake)
}

// ClampHint clamps a function's returned next_wake_hint to [60, MaxWake] (D24.9) — a render may
// hint a longer/shorter sleep but can never escape the bounds.
func (c WakeConfig) ClampHint(hint int) int { return clampWake(hint, c.MaxWake) }

func clampWake(secs, maxWake int) int {
	if maxWake > 0 && secs > maxWake {
		secs = maxWake
	}
	if secs < 60 {
		secs = 60
	}
	return secs
}
