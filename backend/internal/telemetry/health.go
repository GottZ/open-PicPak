package telemetry

import (
	"fmt"
	"strings"
	"time"
)

// VerdictCfg is the Policy=Data threshold set the verdict reads (design 08 §2/§5, D22.10). It is a plain
// struct (no env/DB import to TEST it), built from the ADMIN_TELEMETRY_* env by the owning cmd/admin
// process (W2). health.go has NO DB and NO heavy dependency and is fully table-testable.
type VerdictCfg struct {
	StaleAfter           time.Duration
	BrownoutResetReasons []string
	BrownoutOTARRCodes   []int
	BadBootsWarn         int
	LowBattPct           int
	LowBattIncludesUSB   bool
}

// DefaultVerdictCfg returns the documented defaults (design 22 §5). The env loader (W2) overrides each
// field; tests construct VerdictCfg directly. Defaults-as-data: no magic values buried in the verdict.
func DefaultVerdictCfg() VerdictCfg {
	return VerdictCfg{
		StaleAfter:           3 * time.Hour,
		BrownoutResetReasons: []string{"brownout"},
		BrownoutOTARRCodes:   []int{9},
		BadBootsWarn:         2,
		LowBattPct:           15,
		LowBattIncludesUSB:   false,
	}
}

// Verdict computes the per-device health from ONE row (design 08 §2, D22.4). It is PURE and SINGLE-ROW:
// it never touches the DB and never needs a preceding sample. now is injected so the function stays
// table-testable.
//
//   - No telemetry row at all (s.HasData == false) → NO_DATA, split by the C2-contact clock (D22.12): a
//     device polling C2 within stale_after is "c2_alive" (telemetry just unwired, the §2 current normal),
//     otherwise "silent" (truly dark). This is the only branch that reads C2LastSeen.
//   - Otherwise the most-severe data verdict: OFFLINE_STALE / BROWNOUT / BAD_BOOTS / LOW_BATT / OK.
//
// BROWNOUT here is single-row only (a brownout reset_reason wire string OR a brownout diag_ota_rr code).
// The uptime-rollback brownout signal is a CROSS-ROW corroboration (it needs uptime[n-1]); it is computed
// only over history (RollbackChip) and surfaced as a reason chip, never folded into this single-row verdict.
func Verdict(s Sample, cfg VerdictCfg, now time.Time) (Health, []string) {
	if !s.HasData {
		if s.C2LastSeen != nil && cfg.StaleAfter > 0 && now.Sub(*s.C2LastSeen) <= cfg.StaleAfter {
			return HealthNoData, []string{ReasonC2Alive}
		}
		return HealthNoData, []string{ReasonSilent}
	}

	h := HealthOK
	var reasons []string
	bump := func(to Health, reason string) {
		reasons = append(reasons, reason)
		if to > h {
			h = to
		}
	}

	// OFFLINE_STALE — now-time beyond stale_after. "Offline OR dead" is unresolvable in this axis
	// (firmware §3.7); the view states age, never "live".
	if cfg.StaleAfter > 0 && !s.Time.IsZero() {
		if age := now.Sub(s.Time); age > cfg.StaleAfter {
			bump(HealthOffline, fmt.Sprintf("no report for %s (> stale_after %s)", roundAge(age), cfg.StaleAfter))
		}
	}

	// BROWNOUT — a brownout reset_reason wire string or a brownout diag_ota_rr code (both single-row).
	if rr, ok := s.ResetReason.Get(); ok && containsFold(cfg.BrownoutResetReasons, rr) {
		bump(HealthBrownout, "reset_reason="+rr)
	}
	if code, ok := s.DiagOTARR.Get(); ok && containsInt(cfg.BrownoutOTARRCodes, code) {
		bump(HealthBrownout, fmt.Sprintf("diag_ota_rr=%d (brownout-during-OTA)", code))
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

// RollbackChip reports the cross-row brownout corroboration over two CONSECUTIVE history samples: a true
// uptime drop (cur < prev) AND the current row's reset_reason is a configured brownout (design 08 §2).
// It is a reason CHIP for the device card, never the single-row verdict (D22.4). Sentinel/NULL uptimes on
// either side yield false (a "not measured" sentinel must never read as a drop → false brownout, D22.5).
func RollbackChip(cur, prev Sample, cfg VerdictCfg) bool {
	return uptimeRollback(cur, prev) && reasonIsBrownout(cur, cfg)
}

// uptimeRollback reports a true uptime drop between the previous (older) and current (newer) sample. It
// SKIPS any row whose uptime is NULL or the -1 sentinel (Null.Valid==false after scan), so a "not
// measured this boot" sentinel is never read as a rollback — the documented false-brownout poison case.
func uptimeRollback(cur, prev Sample) bool {
	c, ok1 := cur.Uptime.Get()
	p, ok2 := prev.Uptime.Get()
	if !ok1 || !ok2 { // NULL or -1 sentinel on either side → not a rollback
		return false
	}
	return c < p
}

// reasonIsBrownout reports whether the sample's reset_reason is a configured brownout wire string. The
// rollback term requires this, so a bare uptime drop without a brownout reset never raises a false
// brownout on a benign reboot.
func reasonIsBrownout(s Sample, cfg VerdictCfg) bool {
	rr, ok := s.ResetReason.Get()
	return ok && containsFold(cfg.BrownoutResetReasons, rr)
}

// containsFold is a case-insensitive membership test (reset_name() emits lowercase "brownout" on the
// wire; matching case-insensitively is defensive).
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
