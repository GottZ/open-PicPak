// Package ota is the OTA-management axis (W11 / design 07): the operator surface to
// manage what the SERVER OFFERS — register a built firmware as a firmware_versions
// row, set a channel default, pin/unpin per-serial rollouts, pause/resume/done a
// rollout — and to read, per device, the target version vs the running version vs the
// rollout state. It does NOT push firmware to a device (devices pull; flashing over
// USB is the ssh-rollout axis).
//
// Two backends sit behind one write interface (writer.go): the AdminAPIWriter (the
// target architecture; the backend owns blob storage, sha re-verification and FK /
// precedence integrity) and the interim, flag-gated DirectPGXWriter
// (ota.allow_direct_write, default-off, requires a writable pool — config rule 3 ties
// that to database.read_only=false). The read path is always direct pgx through the
// shared read-only pool (store.go) and a pure precedence resolver (resolve.go).
//
// SHA discipline: every sha256 the tool emits or accepts is validated against
// ^[0-9a-f]{64}$ (64 LOWERCASE hex) before any write. The firmware compares the digest
// with a case-sensitive strcmp and the DB CHECK demands lowercase, so an uppercase hash
// silently fail-closes OTA on every device. The tool rejects it; the DB CHECK is the
// backstop.
//
// The pane (model.go/update.go/view.go) implements the canonical pane.Pane (K1) with
// its own tea.Tick cadence (K3) and addressed-PaneMsg delivery (K2); a nil pool renders
// "no database configured" and never crashes the bench cockpit.
package ota

import (
	"regexp"
	"time"
)

// sha256Re is the lowercase-hex contract shared by the firmware strcmp and the DB
// CHECK (^[0-9a-f]{64}$). The tool validates against it before any write so an
// uppercase/short digest never reaches the database (where the CHECK would reject it
// anyway — this is the earlier, friendlier gate).
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is exactly 64 lowercase hex digits.
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// OTAState is the in-memory snapshot the read Store builds: the offered firmware
// versions, the channels with their defaults, the rollout targets, and the per-device
// target-vs-running view. It is rebuilt on each refresh (no incremental mutation).
type OTAState struct {
	Versions  []FirmwareVersion
	Channels  []Channel
	Rollouts  []Rollout
	Devices   []DeviceOTA // assembled from fleet cache + telemetry running_ver + Resolve
	FetchedAt time.Time
}

// FirmwareVersion is one firmware_versions row (the OTA match key + its integrity
// fields). SHA256 is 64 lowercase hex by the DB CHECK.
type FirmwareVersion struct {
	Version   string
	SHA256    string
	BlobPath  string
	SizeBytes int64
	Notes     string
	CreatedAt time.Time
}

// Channel is one channels row. DefaultVersion is "" when the channel has no default
// (the seeded stable/beta start empty).
type Channel struct {
	Name           string
	DefaultVersion string
}

// Rollout is one rollout_targets row. Serial "*" denotes a channel-fleet target; any
// other value is a per-serial (staging) target. State is active|paused|done; only an
// active target offers a version (resolve.go).
type Rollout struct {
	ID        int64
	Serial    string
	Channel   string
	Version   string
	State     string
	Pinned    bool
	UpdatedAt time.Time
}

const (
	StateActive = "active"
	StatePaused = "paused"
	StateDone   = "done"
)

// ValidState reports whether s is one of the three rollout states the schema allows.
func ValidState(s string) bool {
	switch s {
	case StateActive, StatePaused, StateDone:
		return true
	default:
		return false
	}
}

// DeviceInfo is the minimal per-device input the resolver consumes: the serial, the
// AUTHORITATIVE channel (devices.channel via the fleet cache; falls back to the
// device-claimed telemetry channel when the device is not in the cache), the last
// running version the device reported, and report freshness. It is decoupled from the
// telemetry/fleet row types so resolve.go stays pure (no DB/telemetry import).
type DeviceInfo struct {
	Serial     string
	Label      string
	Channel    string
	RunningVer string
	LastSeen   time.Time
	HasReport  bool
}

// DeviceOTA is one row of the per-device target-vs-running table: the device, the
// predicted resolved target, the matched per-serial rollout (nil when none), and the
// honest "behind" verdict. Behind is true only when the device reported a running
// version AND a target version is offered AND they differ (string inequality, the same
// 32-char match key the firmware uses) — never inferred for a device that never
// reported.
type DeviceOTA struct {
	DeviceInfo
	Resolved ResolvedTarget
	Rollout  *Rollout // the per-serial rollout for this device's channel (active or paused), if any
	Behind   bool
}

// ResolvedTarget is the outcome of the precedence resolver. Version is "" when no
// target is offered (the device stays on whatever it runs). Source names the winning
// precedence step (pinned-serial|serial|channel|default|none). Predicted is true for
// the LOCAL resolver (no backend); the future GET /admin/resolve/{serial} is the
// authoritative resolver and supersedes this when the Admin-API is up.
type ResolvedTarget struct {
	Version   string
	Source    string
	Predicted bool
}
