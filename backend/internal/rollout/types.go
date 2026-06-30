// Package rollout is the SINGLE OTA target resolver, shared across the address-space line: cmd/ingest
// imports it to set the X-Firmware-Version header on the /pp push, cmd/admin imports it to answer
// GET /api/resolve and to validate writes (D20.2). It is write-free and secret-free — the only reason
// it is safe to share between the public parser and the operator process (M5). Keeping ONE resolver
// kills the TUI-07 drift risk structurally: the operator can never see a target the device never gets
// (T12), because both surfaces call the same Resolve.
package rollout

// Source names which precedence tier produced the resolved version.
type Source string

const (
	SourceSerial         Source = "serial"          // a per-serial active rollout_targets row
	SourceFleet          Source = "fleet"           // the channel-wide '*' active rollout_targets row
	SourceChannelDefault Source = "channel-default" // channels.default_version fallback
	SourceNone           Source = "none"            // nothing resolved — no active target, no default
)

// Tier is one precedence step in the rollout walk. The ORDER is policy=data (D20.3): callers pass
// prec []Tier, so the precedence can be re-ordered without a rebuild. The channel default is NOT a
// Tier — it is the implicit final fallback, keyed differently (channels.default_version, not a
// rollout_targets row), and always follows every Tier.
type Tier int

const (
	TierSerial Tier = iota // rollout_targets(serial=<sn>, state='active') — the per-serial staging pin
	TierFleet              // rollout_targets(serial='*',  state='active') — the channel-wide fleet rollout
)

// DefaultPrecedence is the canonical per-serial ▶ fleet order (D20.3): a per-serial active pin beats a
// channel-fleet rollout, which beats the channel default. Default-fixed here; the configurable form is
// OQ (§8) and would live as data, not a code change.
var DefaultPrecedence = []Tier{TierSerial, TierFleet}

// Target is one rollout_targets row relevant to a single device's resolution on one channel: the
// serial ('*' = the whole-fleet wildcard, else the device's own SN), the version it offers, and its
// state. A paused/done row offers nothing — Resolve skips it to the next tier (D20.3). The vestigial
// `pinned` column (0001:84) is NOT modeled: UNIQUE(serial,channel) makes the "pinned per-serial" and
// "per-serial" rows the same single row, so it had no distinct testable semantic (D20.3).
type Target struct {
	Serial  string // '*' (fleet) or a device serial
	Channel string // the channel this target applies to
	Version string // the firmware version it offers
	State   string // 'active' | 'paused' | 'done'
}

// Resolved is the authoritative answer: the version a device should run and which tier produced it.
// Version is "" exactly when Source is SourceNone. The json tags feed admin's GET /api/resolve and the
// operator UI; ingest reads only Version + Source.
type Resolved struct {
	Version string `json:"version"`
	Source  Source `json:"source"`
}
