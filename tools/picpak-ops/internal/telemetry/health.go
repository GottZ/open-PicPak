package telemetry

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// VerdictCfg is the Policy=Data threshold set the verdict reads (design 08 §2/§5).
// It is a plain struct (no config import needed to TEST it) built from config.Telemetry
// via VerdictCfgFrom, so health.go has NO DB and NO heavy dependency and is fully
// table-testable.
type VerdictCfg struct {
	StaleAfter           time.Duration
	BrownoutResetReasons []string
	BrownoutOTARRCodes   []int
	BadBootsWarn         int
	LowBattPct           int
	LowBattIncludesUSB   bool
}

// VerdictCfgFrom lifts the verdict thresholds out of the typed [telemetry] config.
func VerdictCfgFrom(t config.Telemetry) VerdictCfg {
	return VerdictCfg{
		StaleAfter:           t.StaleAfter.D(),
		BrownoutResetReasons: t.BrownoutResetReasons,
		BrownoutOTARRCodes:   t.BrownoutOTARRCodes,
		BadBootsWarn:         t.BadBootsWarn,
		LowBattPct:           t.LowBattPct,
		LowBattIncludesUSB:   t.LowBattIncludesUSB,
	}
}

// Verdict computes the per-device health from one sample (design 08 §2). prev is the
// immediately-PRECEDING (older) sample for the uptime-rollback brownout heuristic, or
// nil when unavailable (a fleet snapshot has only the latest row). now is injected so
// the function stays pure/testable. It returns the most-severe applicable Health plus
// the human reasons that fired (for the detail card). It NEVER touches the DB.
func Verdict(s Sample, prev *Sample, cfg VerdictCfg, now time.Time) (Health, []string) {
	h := HealthOK
	var reasons []string

	bump := func(to Health, reason string) {
		reasons = append(reasons, reason)
		if to > h {
			h = to
		}
	}

	// OFFLINE_STALE — now-time beyond stale_after. "Offline OR dead" is unresolvable
	// in this axis (firmware §3.7); the view states age, never "live".
	if cfg.StaleAfter > 0 && !s.Time.IsZero() {
		if age := now.Sub(s.Time); age > cfg.StaleAfter {
			bump(HealthOffline, fmt.Sprintf("no report for %s (> stale_after %s)", roundAge(age), cfg.StaleAfter))
		}
	}

	// BROWNOUT — any of: a brownout reset_reason wire string, a brownout diag_ota_rr
	// code, or the uptime-rollback heuristic (sentinel rows skipped, see UptimeRollback).
	if rr, ok := s.ResetReason.Get(); ok && containsFold(cfg.BrownoutResetReasons, rr) {
		bump(HealthBrownout, "reset_reason="+rr)
	}
	if code, ok := s.DiagOTARR.Get(); ok && containsInt(cfg.BrownoutOTARRCodes, code) {
		bump(HealthBrownout, fmt.Sprintf("diag_ota_rr=%d (brownout-during-OTA)", code))
	}
	if UptimeRollback(s, prev) && reasonIsBrownout(s, cfg) {
		bump(HealthBrownout, "uptime rollback + brownout reset")
	}

	// BAD_BOOTS — the boot guard is climbing toward safe-mode.
	if cfg.BadBootsWarn > 0 {
		if bb, ok := s.BadBoots.Get(); ok && bb >= cfg.BadBootsWarn {
			bump(HealthBadBoots, fmt.Sprintf("bad_boots=%d (>= %d)", bb, cfg.BadBootsWarn))
		}
	}

	// LOW_BATT — low charge and (not USB-powered unless low_batt_includes_usb).
	if pct, ok := s.BattPct.Get(); ok && pct <= cfg.LowBattPct {
		usb, _ := s.USB.Get()
		if cfg.LowBattIncludesUSB || !usb {
			bump(HealthLowBatt, fmt.Sprintf("batt_pct=%d (<= %d)", pct, cfg.LowBattPct))
		}
	}

	return h, reasons
}

// UptimeRollback reports a true uptime drop between the previous (older) and current
// (newer) sample. It SKIPS any row whose uptime is NULL or the -1 sentinel
// (Null.Valid==false after scan), so a "not measured this boot" sentinel is never read
// as a rollback — the documented false-brownout poison case (design 08 §8). This is a
// pure helper, separately tested so the sentinel-skip is provable in isolation.
func UptimeRollback(s Sample, prev *Sample) bool {
	if prev == nil {
		return false
	}
	cur, ok1 := s.Uptime.Get()
	old, ok2 := prev.Uptime.Get()
	if !ok1 || !ok2 { // NULL or -1 sentinel on either side → not a rollback
		return false
	}
	return cur < old
}

// reasonIsBrownout reports whether the sample's reset_reason is a configured brownout
// wire string. The uptime-rollback term requires this (design 08 §2), so a bare uptime
// drop without a brownout reset never raises a false brownout on a benign reboot.
func reasonIsBrownout(s Sample, cfg VerdictCfg) bool {
	rr, ok := s.ResetReason.Get()
	return ok && containsFold(cfg.BrownoutResetReasons, rr)
}

// containsFold is a case-insensitive membership test (reset_name() emits lowercase
// "brownout" on the wire; matching case-insensitively is defensive).
func containsFold(set []string, v string) bool {
	for _, s := range set {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// containsInt is an int membership test.
func containsInt(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// roundAge rounds a duration to a readable granularity for the reason text.
func roundAge(d time.Duration) time.Duration {
	switch {
	case d >= 24*time.Hour:
		return d.Round(time.Hour)
	case d >= time.Hour:
		return d.Round(time.Minute)
	default:
		return d.Round(time.Second)
	}
}
