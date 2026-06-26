package sshhost

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// inventoryPane (kind pane.KindHosts) is the "which PicPak is where" view. It is a
// data sink over the sshhost axis: it folds HostPollMsg / HostStateMsg from its
// Update into rendered host▸device rows, and reads HostRegistry.Snapshot() for the
// first paint. It opens no transport itself — the registry's workers do all I/O and
// reach this pane only through addressed PaneMsg (K2).
//
// It labels a present device as USB-attachment, NOT device-awake (§9 "present ≠
// awake"): a row says "present" because the by-id symlink exists, never that the
// PicPak is interactable (it may be asleep; the USB console tears down ~60 ms into
// a run_cycle).
type inventoryPane struct {
	pane.BasePane
	reg *HostRegistry

	// rows mirror the latest per-host state, keyed by host name (config order kept
	// in `order`). Updated in Update from HostPollMsg / HostStateMsg.
	mu    sync.Mutex
	order []string
	rows  map[string]HostState

	dirty bool // unseen update since last focused (drives the chrome unread marker)
}

// Factory returns the pane.KindHosts factory plus an accessor for the lazily-built
// HostRegistry, wired the wm-shell §4.5 way: the registry is constructed the first
// time a hosts pane is spawned, using that pane's own PaneID as the inventory target
// and the pane's already-injected Sender (live after SetSender, before Init). The
// returned getReg lets cmd/picpak-ops reach the registry for teardown (Stop) and so
// later axes (build/flash/console) can consume the same single transport (K9).
//
// All hosts panes spawned in one process share ONE registry (one worker set per
// host); a second hosts pane re-renders the same inventory.
func Factory() (factory func(pane.BasePane, *config.Config) pane.Pane, getReg func() *HostRegistry) {
	var (
		mu  sync.Mutex
		reg *HostRegistry
	)
	getReg = func() *HostRegistry {
		mu.Lock()
		defer mu.Unlock()
		return reg
	}
	factory = func(base pane.BasePane, cfg *config.Config) pane.Pane {
		mu.Lock()
		if reg == nil {
			// Build the registry with this pane as the addressed inventory target and
			// its injected Sender; start the focus-independent poll workers.
			reg = NewHostRegistry(cfg, base.Sender(), base.ID())
			reg.Start()
		}
		r := reg
		mu.Unlock()

		p := &inventoryPane{
			BasePane: base,
			reg:      r,
			rows:     map[string]HostState{},
		}
		// Seed the first paint from the registry snapshot (pull side, §4.3).
		snap := r.Snapshot()
		for _, hs := range snap.Hosts {
			p.order = append(p.order, hs.Host)
			p.rows[hs.Host] = hs
		}
		return p
	}
	return factory, getReg
}

// SharedFactory returns a hosts-pane factory bound to a pre-built, app-level shared
// HostRegistry (the §4.5 promotion: the registry is constructed once at startup in
// cmd/picpak-ops so flash always has a transport even when no hosts pane is open,
// and both the hosts pane and the flash pane share the ONE registry — K9). On spawn
// the pane adopts the live poll stream by re-targeting the registry to its own id
// (SetInventoryPane), then seeds its first paint from the registry snapshot. Unlike
// Factory(), this neither builds nor starts the registry — cmd/picpak-ops owns its
// lifecycle (Start at boot, Stop on teardown).
func SharedFactory(reg *HostRegistry) func(pane.BasePane, *config.Config) pane.Pane {
	return func(base pane.BasePane, cfg *config.Config) pane.Pane {
		reg.SetInventoryPane(base.ID())
		p := &inventoryPane{
			BasePane: base,
			reg:      reg,
			rows:     map[string]HostState{},
		}
		snap := reg.Snapshot()
		for _, hs := range snap.Hosts {
			p.order = append(p.order, hs.Host)
			p.rows[hs.Host] = hs
		}
		return p
	}
}

// Init opens nothing: the registry's workers (started at registry construction)
// already stream HostPollMsg / HostStateMsg to this pane's id via the Sender. The
// pane has no tea.Tick of its own — its cadence lives in the worker goroutines.
func (p *inventoryPane) Init() tea.Cmd { return nil }

// Update folds the axis messages into row state. Addressed payloads (HostPollMsg /
// HostStateMsg) are always delivered, focused or background (K2), so the inventory
// stays current even while another pane is foregrounded.
func (p *inventoryPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {
	case HostPollMsg:
		p.mu.Lock()
		hs := p.rows[m.Host]
		hs.Host = m.Host
		hs.State = m.State
		hs.Devices = m.Devices
		hs.Err = m.Err
		hs.At = m.At
		p.ensureHostLocked(m.Host)
		p.rows[m.Host] = hs
		if !p.Focused() {
			p.dirty = true
		}
		p.mu.Unlock()
		return p, nil

	case HostStateMsg:
		p.mu.Lock()
		hs := p.rows[m.Host]
		hs.Host = m.Host
		hs.State = m.State
		if m.Err != nil {
			hs.Err = m.Err
		}
		hs.At = m.At
		p.ensureHostLocked(m.Host)
		p.rows[m.Host] = hs
		if !p.Focused() {
			p.dirty = true
		}
		p.mu.Unlock()
		return p, nil
	}
	return p, nil
}

// ensureHostLocked appends a host to the stable order on first sight (caller holds mu).
func (p *inventoryPane) ensureHostLocked(host string) {
	if _, ok := p.rows[host]; ok {
		return
	}
	p.order = append(p.order, host)
}

// SetFocused clears the unread marker when the pane gains focus (cosmetic; never
// opens/closes transport, R5).
func (p *inventoryPane) SetFocused(focused bool) {
	p.BasePane.SetFocused(focused)
	if focused {
		p.mu.Lock()
		p.dirty = false
		p.mu.Unlock()
	}
}

// View renders one block per host: a host header with its connection badge, then
// one row per attached device (tty, descriptor short form, mapped/unmapped). A
// device row labels presence as USB-attachment, not liveness (§9).
func (p *inventoryPane) View() string {
	p.mu.Lock()
	order := append([]string(nil), p.order...)
	rows := make(map[string]HostState, len(p.rows))
	for k, v := range p.rows {
		rows[k] = v
	}
	p.mu.Unlock()

	var b strings.Builder
	b.WriteString("hosts — which PicPak is where\n")
	b.WriteString("(present = USB-attached, not necessarily awake)\n\n")

	if len(order) == 0 {
		b.WriteString("No hosts configured.\n")
		return b.String()
	}

	for _, host := range order {
		hs := rows[host]
		fmt.Fprintf(&b, "%s  [%s]", host, hs.State.String())
		if hs.Err != nil {
			fmt.Fprintf(&b, "  %s", connHint(hs.Err))
		}
		b.WriteByte('\n')

		if hs.State == StateDisabled {
			b.WriteString("    (disabled)\n")
		} else if len(hs.Devices) == 0 {
			b.WriteString("    no PicPak present\n")
		} else {
			// Stable device order by tty so the view does not jitter.
			devs := append([]DiscoveredDevice(nil), hs.Devices...)
			sort.Slice(devs, func(i, j int) bool { return devs[i].TTY < devs[j].TTY })
			for _, d := range devs {
				fmt.Fprintf(&b, "    %s  %s  %s\n", d.TTY, mapLabel(d), shortDesc(d.Descriptor))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Meta gives the pane a stable title and an honest status: Working while any host
// is mid-poll-cycle is not tracked here (poll is brief); the pane is Active when any
// host is up with a device present, Quiet when hosts are up but empty, Error when a
// host is down, else Idle.
func (p *inventoryPane) Meta() pane.PaneMeta {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := pane.StatusIdle
	anyUp, anyDevice, anyDown := false, false, false
	for _, hs := range p.rows {
		switch hs.State {
		case StateUp:
			anyUp = true
			if len(hs.Devices) > 0 {
				anyDevice = true
			}
		case StateDown:
			anyDown = true
		}
	}
	switch {
	case anyDevice:
		status = pane.StatusActive
	case anyDown:
		status = pane.StatusError
	case anyUp:
		status = pane.StatusQuiet
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  string(pane.KindHosts),
		Status: status,
		Dirty:  p.dirty,
	}
}

// Close releases the pane's view of the registry. It does NOT stop the registry:
// the registry is process-wide (shared by future build/flash/console consumers) and
// is torn down by cmd/picpak-ops via the getReg accessor on app teardown. A hosts
// pane closing must not kill the transport the other axes ride on.
func (p *inventoryPane) Close() error { return nil }

// mapLabel renders the device's identity column honestly: "mapped:<serial/label>"
// when the best-effort backend join produced one, else "unmapped" (the common case,
// §1.1) — never implying an identity the host cannot observe.
func mapLabel(d DiscoveredDevice) string {
	if d.Serial != "" {
		return "mapped:" + d.Serial
	}
	if d.Label != "" {
		return "mapped:" + d.Label
	}
	return "unmapped"
}

// shortDesc trims the by-id descriptor to a compact tail for the row; the full
// descriptor is the gate key, this is display-only.
func shortDesc(desc string) string {
	const max = 40
	if len(desc) <= max {
		return desc
	}
	return "…" + desc[len(desc)-max:]
}

// connHint turns a poll error into a short actionable hint, distinguishing a host-
// key-verification failure (operator must seed known_hosts) from a plain connect
// failure (host down) — §9 BatchMode first-contact risk.
func connHint(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "host key verification failed"),
		strings.Contains(s, "remote host identification has changed"):
		return "(host-key: seed known_hosts)"
	case strings.Contains(s, "connection refused"),
		strings.Contains(s, "could not resolve"),
		strings.Contains(s, "no route to host"),
		strings.Contains(s, "connection timed out"),
		strings.Contains(s, "operation timed out"):
		return "(unreachable)"
	default:
		return "(poll failed)"
	}
}
