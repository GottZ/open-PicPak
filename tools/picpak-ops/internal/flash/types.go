// Package flash is axis 05 (W7): the esptool firmware rollout. It builds the
// write-only flash recipe from the config-owned build.artifacts, enforces the ONE
// inviolable safety property (factory NVS @ 0x9000 is never erased or overwritten)
// in validateWriteSet, stages artifacts onto the device's host, and drives esptool
// per device with an explicit per-device result — never a silent batch glyph.
//
// It owns no transport (K9): the esptool write rides sshhost.HostRegistry.Run and
// staging rides the sshhost Runner; flash only builds argv, stages bytes through the
// transport, and parses output. Background goroutines reach the pane ONLY via the
// injected pane.Sender as an addressed app.PaneMsg (K2).
//
// Air-gap: no host, port, MAC, serial, or offset literal lives here. Targets and
// ports arrive from the VID:PID-gated discovery inventory at runtime; offsets are
// config (build.artifacts); 303a:1001 is the public Espressif gate, read from config
// by the ssh-host axis, never a literal here.
package flash

import (
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// TargetID is the per-device identity in the matrix (host+tty). Runtime-derived from
// the gated inventory, never a code literal (air-gap).
type TargetID string

// Target is one device selected for flashing: its host, the VID-gated device record
// (carries the resolved tty = the port), and a stable id for the matrix.
type Target struct {
	ID     TargetID
	Host   string
	Device sshhost.DiscoveredDevice
}

// Port returns the device node esptool writes to and that a console pane must
// release first (K6) — the gated tty from discovery, never a literal.
func (t Target) Port() string { return t.Device.TTY }

// NewTarget builds a Target from a gated device, deriving a stable id from
// (host, tty) — the same robust key discovery uses (MAC may be absent).
func NewTarget(dev sshhost.DiscoveredDevice) Target {
	return Target{ID: TargetID(dev.Host + ":" + dev.TTY), Host: dev.Host, Device: dev}
}

// Phase is a per-device lifecycle stage surfaced in the matrix row. A device is
// non-terminal (StatusWorking at the pane) until PhaseDone or PhaseFailed.
type Phase int

const (
	PhasePending    Phase = iota // selected, not yet started
	PhaseValidating              // building + validating the write set (the safety gate)
	PhaseStaging                 // copying artifacts to the host
	PhaseConfirming              // re-asserting the VID:PID gate on the host
	PhaseFlashing                // esptool write-flash in progress
	PhaseDone                    // success: every file hash-verified AND exit 0
	PhaseFailed                  // terminal failure with a fail class
)

// String renders the phase for the matrix.
func (p Phase) String() string {
	switch p {
	case PhasePending:
		return "pending"
	case PhaseValidating:
		return "validating"
	case PhaseStaging:
		return "staging"
	case PhaseConfirming:
		return "confirming"
	case PhaseFlashing:
		return "flashing"
	case PhaseDone:
		return "done"
	case PhaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Terminal reports whether the phase is a final state.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// FailClass is the recovery taxonomy: each class maps to an operator hint and a
// retry decision. NONE is the success sentinel.
type FailClass int

const (
	FailNone     FailClass = iota
	FailValidate           // write set / argv rejected by the NVS/erase guard (fail-closed)
	FailStage              // artifact staging failed (scp/sha/copy)
	FailGate               // HostRegistry.Confirm rejected the device (VID:PID / staleness)
	FailPortBusy           // device node could not be opened (held / perms)
	FailSync               // esptool could not sync with the ROM
	FailHash               // a file's hash did not verify (or not all files verified)
	FailReset              // post-write reset/leave failed
	FailTimeout            // the per-device run timeout fired
	FailExit               // generic non-zero exit not otherwise classified
)

// String renders the class for the matrix + diagnostics.
func (c FailClass) String() string {
	switch c {
	case FailNone:
		return "ok"
	case FailValidate:
		return "validate"
	case FailStage:
		return "stage"
	case FailGate:
		return "gate"
	case FailPortBusy:
		return "port-busy"
	case FailSync:
		return "sync-fail"
	case FailHash:
		return "hash-fail"
	case FailReset:
		return "reset-fail"
	case FailTimeout:
		return "timeout"
	case FailExit:
		return "exit"
	default:
		return "unknown"
	}
}

// Retryable reports whether an auto-retry (retry_auto) is sensible for this class.
// Only the transient transport classes retry; a validate/gate/hash failure is not a
// transient condition and must not be silently re-attempted.
func (c FailClass) Retryable() bool {
	switch c {
	case FailPortBusy, FailSync, FailReset, FailTimeout:
		return true
	default:
		return false
	}
}

// DeviceResult is one device's live + terminal state in the matrix. It is mutated
// only on the message loop (the pane folds controller messages into it), so it needs
// no lock.
type DeviceResult struct {
	Target   Target
	Phase    Phase
	Pct      int       // current esptool write percentage (cosmetic, live)
	Verified int       // count of files whose hash verified
	NumFiles int       // files in the write set (the verified-all denominator)
	OK       bool      // success: set only on PhaseDone
	Class    FailClass // fail class on PhaseFailed
	Hint     string    // operator recovery hint
	Tail     []string  // bounded esptool output tail for the failure box
}
