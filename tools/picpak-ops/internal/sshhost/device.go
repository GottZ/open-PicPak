// Package sshhost is axis 03: the host-connectivity + device-polling substrate
// every device-touching feature stands on (build, flash, console, telemetry).
//
// It owns three things and nothing else: transport (run a command on a remote or
// local host via the operator's system ssh / os/exec, with ControlMaster reuse),
// discovery (enumerate which PicPak is attached to which host behind a strict
// VID:PID gate), and long-running output streaming (a *Run whose stdout/stderr
// arrive as addressed tea.Msg, never blocking Update). It does NOT own the flash
// recipe, the console protocol, or telemetry — those consume the Runner and
// HostRegistry defined here (03-ssh-host.md §1; Masterplan K9: this is the ONE
// transport, no separate internal/exec).
//
// Bubble Tea bridge (K2): background goroutines reach the UI ONLY by calling the
// injected pane.Sender with an addressed PaneMsg targeting the inventory pane.
// No goroutine writes Model state; no private channel; no waitForMsg Cmd.
//
// Air-gap: every host, port glob, VID:PID, path, and credential is config
// (Policy=Data). This package carries no real identifier — the public Espressif
// USB-Serial-JTAG VID:PID 303a:1001 is read from config, never a code literal.
package sshhost

import (
	"sync"
	"time"
)

// ConnState is a host's connection-state-machine state, surfaced so the inventory
// pane can be honest about a host that is connecting / backing off / down rather
// than implying every host is reachable.
type ConnState int

const (
	// StateConnecting: a poll is in flight and the host has not yet succeeded or
	// failed enough times to be declared up or down (initial state).
	StateConnecting ConnState = iota
	// StateUp: the last poll succeeded; the host is reachable.
	StateUp
	// StateBackoff: a poll failed but the consecutive-failure count is still below
	// the fail threshold; the worker is waiting out a backoff before the next try.
	StateBackoff
	// StateDown: consecutive failures reached the configured fail threshold; the
	// host is shown down until a poll succeeds again.
	StateDown
	// StateDisabled: the host entry exists but enabled=false; no worker polls it.
	StateDisabled
)

// String renders the state for diagnostics and row rendering.
func (s ConnState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateUp:
		return "up"
	case StateBackoff:
		return "backoff"
	case StateDown:
		return "down"
	case StateDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// DiscoveredDevice is one PicPak observed on a host by a poll. The robust identity
// key is (Host, TTY) per §1.1 — MAC/serial enrichment is best-effort and EXPECTED
// to miss with current firmware, so a device with an empty MAC/Serial/Label is the
// normal case, not an edge case. Presence here means USB-attachment, NOT
// device-awake (§9 "present ≠ awake"): the device may be asleep and unconsumable.
type DiscoveredDevice struct {
	Host       string // hosts[].name the device was seen on
	TTY        string // resolved real device path (e.g. /dev/ttyACM0); the stable key with Host
	Descriptor string // the /dev/serial/by-id symlink name that matched poll.match_regex
	MAC        string // best-effort, from the descriptor via mac_regex; "" when absent (UNVERIFIED on-device)

	// Enrichment (best-effort backend join; usually empty with current FW, §1.1).
	Serial  string // factory serial if a backend join succeeded; "" = unmapped (the common case)
	Label   string // human label from the backend/config; "" = unmapped
	Channel string // OTA channel from the backend; "" = unmapped

	// Confirmed reports whether a strict VID:PID confirm has passed for this device
	// (set by Confirm()/the confirm cache). Poll alone never sets it true — poll is
	// descriptor-match only; the hard gate runs before any open/flash.
	Confirmed bool
}

// Mapped reports whether the best-effort identity join produced a serial/label.
// The common display state is unmapped (§1.1), so the pane shows this honestly.
func (d DiscoveredDevice) Mapped() bool { return d.Serial != "" || d.Label != "" }

// Key is the robust identity key for a device: (host, tty). MAC is not part of it
// (it may be absent — §1.1), so two PicPaks on one host are still distinct by tty.
func (d DiscoveredDevice) Key() string { return d.Host + "\x00" + d.TTY }

// MACLookup is the best-effort identity-enrichment seam (§1.1, §6 telemetry dep).
// Given a host-observed MAC it returns (serial, label, channel, ok). It is supplied
// by the telemetry axis (axis 08, a later wave) over backend.devices; sshhost never
// imports the DB. With current firmware it is EXPECTED to miss (devices.mac is a
// legacy MAC-suffix and frequently NULL), so ok=false is the common case and an
// unmapped device is the normal display state — enrichment never gates connectivity.
type MACLookup func(mac string) (serial, label, channel string, ok bool)

// enrich applies a best-effort MACLookup to a device, filling Serial/Label/Channel
// when the lookup hits. A nil lookup or a miss leaves the device unmapped (the
// common case). The join is a SUFFIX match (OQ 5): the host-side descriptor MAC (if
// present at all) is a full MAC while devices.mac is a legacy suffix, so a direct
// equality join never matches — the lookup itself owns the suffix logic; this helper
// just feeds it the device MAC and folds a hit back in.
func enrich(dev DiscoveredDevice, lookup MACLookup) DiscoveredDevice {
	if lookup == nil || dev.MAC == "" {
		return dev
	}
	if serial, label, channel, ok := lookup(dev.MAC); ok {
		dev.Serial = serial
		dev.Label = label
		dev.Channel = channel
	}
	return dev
}

// HostState is a host's current poll result + connection state. It is the unit the
// inventory pane folds into a host row. A frozen value carried inside a HostPollMsg
// / HostStateMsg; not mutated after send (the worker builds a fresh one each emit).
type HostState struct {
	Host    string
	State   ConnState
	Devices []DiscoveredDevice
	Err     error     // last poll error (nil when up); surfaced as an actionable hint
	LastOK  time.Time // time of the last successful poll (zero = never)
	At      time.Time // time this state was produced
	Fails   int       // consecutive-failure count (drives backoff + the down threshold)
}

// Inventory is an immutable, lock-free snapshot of every host's current state,
// returned by HostRegistry.Snapshot() for the inventory pane's first paint and any
// re-render that did not buffer every delta. Snapshot copies the live map so the
// caller can read it on the message loop without locking.
type Inventory struct {
	Hosts []HostState // one per configured host, in config order
	At    time.Time   // when the snapshot was taken
}

// inventoryStore is the registry's internal, lock-protected source of truth that
// Snapshot() copies from. Workers update their own host entry under the lock; the
// pane reads via Snapshot(). It keeps host order stable (config order).
type inventoryStore struct {
	mu     sync.RWMutex
	order  []string             // host names in config order (stable)
	states map[string]HostState // host name → its latest state
}

func newInventoryStore(hostOrder []string) *inventoryStore {
	st := &inventoryStore{
		order:  append([]string(nil), hostOrder...),
		states: make(map[string]HostState, len(hostOrder)),
	}
	for _, h := range hostOrder {
		st.states[h] = HostState{Host: h, State: StateConnecting}
	}
	return st
}

// set replaces a host's state (called by its worker under the store lock).
func (s *inventoryStore) set(hs HostState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[hs.Host] = hs
}

// snapshot returns an immutable copy in stable host order.
func (s *inventoryStore) snapshot() Inventory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Inventory{Hosts: make([]HostState, 0, len(s.order)), At: time.Now()}
	for _, name := range s.order {
		hs := s.states[name]
		// Copy the device slice so the caller cannot mutate the store's backing.
		if len(hs.Devices) > 0 {
			devs := make([]DiscoveredDevice, len(hs.Devices))
			copy(devs, hs.Devices)
			hs.Devices = devs
		}
		out.Hosts = append(out.Hosts, hs)
	}
	return out
}
