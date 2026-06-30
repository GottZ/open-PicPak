package rollout

// Resolve walks the precedence tiers for a device on channel `ch` and returns the first match (D20.3).
// It is pure — no DB, no secrets — so it is the single resolver shared across the address-space line
// (D20.2): ingest's served X-Firmware-Version and admin's GET /api/resolve can never diverge (T12).
//
// Contract: `rs` are the rollout_targets rows ALREADY SCOPED to this device — its own per-serial row
// plus the channel '*' fleet row (read.go fetches `serial IN (sn,'*')`); any non-'*' row in `rs` IS
// the device's per-serial target. `chDefault` is channels.default_version for `ch` ("" if unset).
// `prec` is the tier order (DefaultPrecedence = per-serial ▶ fleet), policy=data so it re-orders
// without a rebuild.
//
// Only an 'active' row offers its version; a paused/done row is SKIPPED to the next tier (D20.3) — it
// never short-circuits the walk, so a paused per-serial pin falls through to the fleet rollout, then
// the channel default. After every Tier is exhausted, the channel default is the final fallback; if
// that too is empty, the result is SourceNone / "".
func Resolve(ch string, rs []Target, chDefault string, prec []Tier) Resolved {
	for _, tier := range prec {
		for _, t := range rs {
			if t.Channel != ch || t.State != "active" {
				continue // wrong channel, or paused/done offers nothing (D20.3)
			}
			switch tier {
			case TierSerial:
				if t.Serial != "*" {
					return Resolved{Version: t.Version, Source: SourceSerial}
				}
			case TierFleet:
				if t.Serial == "*" {
					return Resolved{Version: t.Version, Source: SourceFleet}
				}
			}
		}
	}
	if chDefault != "" {
		return Resolved{Version: chDefault, Source: SourceChannelDefault}
	}
	return Resolved{Source: SourceNone}
}
