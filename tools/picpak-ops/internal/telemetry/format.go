package telemetry

import (
	"fmt"
	"time"
)

// naLabel is the single rendering of "not measured / unknown".
const naLabel = "n/a"

// BattString renders a batt_mv Null[int] per the configured unit ("V" → "4.18 V",
// "mV" → "4180 mV"); an absent/sentinel value is n/a.
func BattString(mv Null[int], unit string) string {
	v, ok := mv.Get()
	if !ok {
		return naLabel
	}
	if unit == "mV" {
		return fmt.Sprintf("%d mV", v)
	}
	return fmt.Sprintf("%.2f V", float64(v)/1000)
}

// PctString renders a batt_pct Null[int] as "82%" or n/a.
func PctString(pct Null[int]) string {
	v, ok := pct.Get()
	if !ok {
		return naLabel
	}
	return fmt.Sprintf("%d%%", v)
}

// IntString renders a Null[int] verbatim or n/a.
func IntString(n Null[int]) string {
	v, ok := n.Get()
	if !ok {
		return naLabel
	}
	return fmt.Sprintf("%d", v)
}

// Int64String renders a Null[int64] verbatim or n/a.
func Int64String(n Null[int64]) string {
	v, ok := n.Get()
	if !ok {
		return naLabel
	}
	return fmt.Sprintf("%d", v)
}

// BoolString renders a Null[bool] as yes/no or n/a (used for usb).
func BoolString(b Null[bool]) string {
	v, ok := b.Get()
	if !ok {
		return naLabel
	}
	if v {
		return "yes"
	}
	return "no"
}

// UptimeString renders uptime_ms (Null[int64]) as a compact duration, or n/a for a
// NULL/sentinel value (so a -1 sentinel never prints as a bogus "-1ms").
func UptimeString(ms Null[int64]) string {
	v, ok := ms.Get()
	if !ok {
		return naLabel
	}
	d := time.Duration(v) * time.Millisecond
	return d.Round(time.Second).String()
}

// Age renders the wall-clock age of a report time as "5m ago" / "3d ago" (design 08
// §2: last-seen age = now - max(telemetry.time), since time is server-set). A zero
// time is "never"; a future time clamps to "just now".
func Age(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// resetReasonNames glosses the firmware reset_name() wire strings (dev.c:89-103) to a
// readable label; an unknown reason is rendered verbatim so a future wire string never
// becomes a blank.
var resetReasonNames = map[string]string{
	"poweron":   "power-on",
	"sw":        "software reset",
	"panic":     "panic",
	"int_wdt":   "interrupt WDT",
	"task_wdt":  "task WDT",
	"wdt":       "watchdog",
	"deepsleep": "deep-sleep wake",
	"usb":       "USB reset",
	"brownout":  "brownout",
	"other":     "other",
}

// ResetReasonString humanizes a reset_reason Null[string], or n/a when absent.
func ResetReasonString(rr Null[string]) string {
	v, ok := rr.Get()
	if !ok {
		return naLabel
	}
	if name, found := resetReasonNames[v]; found {
		return name
	}
	return v
}

// ChannelMismatch returns the mismatch badge text when the device-CLAIMED channel
// (telemetry.channel) differs from the authoritative assigned channel (devices.channel)
// and the warning is enabled; otherwise "". An empty claimed/assigned side is treated
// as "no claim to compare" and never flags.
func ChannelMismatch(claimed, assigned string, warn bool) string {
	if !warn || claimed == "" || assigned == "" || claimed == assigned {
		return ""
	}
	return fmt.Sprintf("device reports %q, assigned %q", claimed, assigned)
}
