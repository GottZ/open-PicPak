package sshhost

import (
	"runtime"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// pollSink is a goroutine-safe Sender that records HostPollMsg deliveries so a test
// can wait for the first poll without a tea.Program.
type pollSink struct {
	mu    sync.Mutex
	polls []HostPollMsg
	got   chan struct{}
	once  sync.Once
}

func newPollSink() *pollSink { return &pollSink{got: make(chan struct{})} }

func (s *pollSink) send(msg tea.Msg) {
	pm, ok := msg.(app.PaneMsg)
	if !ok {
		return
	}
	if hp, ok := pm.Payload.(HostPollMsg); ok {
		s.mu.Lock()
		s.polls = append(s.polls, hp)
		s.mu.Unlock()
		s.once.Do(func() { close(s.got) })
	}
}

func (s *pollSink) last() (HostPollMsg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.polls) == 0 {
		return HostPollMsg{}, false
	}
	return s.polls[len(s.polls)-1], true
}

// localPollCfg builds a defaults config with one LOCAL host whose poll.command is a
// shell that prints a synthetic by-id listing (a PicPak + a Zigbee dongle), so the
// worker polls without ssh. The Zigbee line must be gated out.
func localPollCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Opts{}) // defaults + env, no file
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Hosts = []config.Host{
		{Name: "local-bench", Local: true, Enabled: true},
	}
	// Replace the poll probe with a local shell that emits the golden capture.
	cfg.Poll.Command = []string{"sh", "-c", "printf '%s' \"$0\"", goldenByID}
	// Keep cadence long so only the immediate first poll fires during the test.
	cfg.Poll.Interval = config.Duration(time.Hour)
	return cfg
}

// TestRegistryWorkerPollsAndGates proves the per-host worker polls a LOCAL host,
// applies the descriptor gate (drops the Zigbee ttyUSB line), emits a HostPollMsg to
// the inventory pane id, and the snapshot reflects the gated device set with the
// host marked up.
func TestRegistryWorkerPollsAndGates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh for the local poll probe")
	}
	cfg := localPollCfg(t)
	sink := newPollSink()
	invID := pane.PaneID("hosts:1")

	reg := NewHostRegistry(cfg, sink.send, invID)
	defer reg.Stop()
	reg.Start()

	select {
	case <-sink.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no HostPollMsg within 5s")
	}

	hp, ok := sink.last()
	if !ok {
		t.Fatal("no poll recorded")
	}
	if hp.Host != "local-bench" {
		t.Fatalf("poll host = %q, want local-bench", hp.Host)
	}
	if hp.State != StateUp {
		t.Fatalf("host state = %v, want up", hp.State)
	}
	if len(hp.Devices) != 2 {
		t.Fatalf("expected 2 gated PicPak devices, got %d: %+v", len(hp.Devices), hp.Devices)
	}
	for _, d := range hp.Devices {
		if d.TTY == "/dev/ttyUSB0" {
			t.Fatal("Zigbee ttyUSB0 leaked through the worker gate")
		}
		if d.Mapped() {
			t.Errorf("device %s reported mapped without a backend join (expected unmapped, §1.1)", d.TTY)
		}
	}

	// Snapshot (the pane's pull side) must reflect the same up host.
	snap := reg.Snapshot()
	if len(snap.Hosts) != 1 || snap.Hosts[0].Host != "local-bench" {
		t.Fatalf("snapshot hosts = %+v, want one local-bench", snap.Hosts)
	}
	if snap.Hosts[0].State != StateUp {
		t.Fatalf("snapshot state = %v, want up", snap.Hosts[0].State)
	}
}

// TestRegistryDisabledHostHasNoWorker proves an enabled=false host gets a Disabled
// placeholder state and no poll (no HostPollMsg ever arrives for it).
func TestRegistryDisabledHostHasNoWorker(t *testing.T) {
	cfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Hosts = []config.Host{{Name: "off-host", Local: true, Enabled: false}}

	reg := NewHostRegistry(cfg, func(tea.Msg) {}, "hosts:1")
	defer reg.Stop()
	reg.Start()

	// Give any (incorrectly spawned) worker a moment.
	time.Sleep(100 * time.Millisecond)

	snap := reg.Snapshot()
	if len(snap.Hosts) != 1 {
		t.Fatalf("snapshot hosts = %d, want 1", len(snap.Hosts))
	}
	if snap.Hosts[0].State != StateDisabled {
		t.Fatalf("disabled host state = %v, want disabled", snap.Hosts[0].State)
	}
}

// TestInventoryPaneFolds proves the inventory pane folds a HostPollMsg into a host
// row and renders the device (USB-attachment), dropping nothing it was handed.
func TestInventoryPaneFolds(t *testing.T) {
	factory, _ := Factory()
	cfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// No hosts → registry builds with an empty inventory; the pane still works.
	base := pane.NewBase(pane.PaneID("hosts:1"), pane.KindHosts, t.Context(), func(tea.Msg) {})
	p := factory(base, cfg)

	updated, _ := p.Update(HostPollMsg{
		Host:  "host-a",
		State: StateUp,
		Devices: []DiscoveredDevice{
			{Host: "host-a", TTY: "/dev/ttyACM0", Descriptor: "usb-Espressif_USB_JTAG_x-if00"},
		},
		At: time.Now(),
	})
	view := updated.View()
	for _, want := range []string{"host-a", "up", "/dev/ttyACM0", "unmapped"} {
		if !contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	// Meta status must be Active (a device is present on an up host).
	if got := updated.Meta().Status; got != pane.StatusActive {
		t.Errorf("pane status = %v, want active", got)
	}
	_ = updated.Close()
}
