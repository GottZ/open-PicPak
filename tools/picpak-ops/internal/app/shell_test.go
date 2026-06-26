package app

import (
	"context"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// --- test fixtures -----------------------------------------------------------

// recPane records every payload its Update saw and counts Close() calls. It also
// records a shared close-order log (pointer shared across panes) so the teardown
// test can assert reverse-spawn order.
type recPane struct {
	pane.BasePane
	mu        sync.Mutex
	seen      []tea.Msg
	closes    int
	status    pane.PaneStatus
	closeLog  *[]pane.PaneID
	closeLogM *sync.Mutex
}

func (p *recPane) Init() tea.Cmd { return nil }

func (p *recPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	p.mu.Lock()
	p.seen = append(p.seen, msg)
	p.mu.Unlock()
	return p, nil
}

func (p *recPane) View() string { return "" }

func (p *recPane) Meta() pane.PaneMeta {
	m := p.BasePane.Meta()
	m.Status = p.status
	return m
}

func (p *recPane) Close() error {
	p.closes++
	if p.closeLog != nil {
		p.closeLogM.Lock()
		*p.closeLog = append(*p.closeLog, p.ID())
		p.closeLogM.Unlock()
	}
	return nil
}

func (p *recPane) sawCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}

// testModel builds a root Model on defaults with a no-op Sender and no reload.
func testModel(t *testing.T) (*Model, context.CancelFunc) {
	t.Helper()
	cfg, err := config.Load(config.Opts{}) // defaults + env, no file
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := New(cfg, ctx, cancel)
	m.SetSender(func(tea.Msg) {}) // bridge already wired; this makes it live
	return m, cancel
}

// --- background-delivery (THE core property) ---------------------------------

// TestBackgroundDeliveryToHiddenPane registers two panes, focuses A, routes an
// addressed PaneMsg to B, and asserts B's Update saw it even though B is hidden.
// This is the load-bearing property of the whole shell (K2).
func TestBackgroundDeliveryToHiddenPane(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	var paneA, paneB *recPane
	m.RegisterFactory("rec-a", func(b pane.BasePane, _ *config.Config) pane.Pane {
		paneA = &recPane{BasePane: b}
		return paneA
	})
	m.RegisterFactory("rec-b", func(b pane.BasePane, _ *config.Config) pane.Pane {
		paneB = &recPane{BasePane: b}
		return paneB
	})

	// Give the layout a size so panes are sized; spawn A (focused) then B (hidden).
	m.onResize(80, 24)
	_ = m.spawn("rec-a", nil, true)
	_ = m.spawn("rec-b", nil, false)

	if paneA == nil || paneB == nil {
		t.Fatal("factories did not produce panes")
	}
	idA, idB := paneA.ID(), paneB.ID()

	// Focused pane must be A; B is hidden/background.
	if focused, _ := m.layout.Tabs().Focused(); focused != idA {
		t.Fatalf("focused = %q, want A=%q", focused, idA)
	}

	// Route an addressed message to the HIDDEN pane B.
	type chunk struct{ n int }
	m.Update(PaneMsg{To: idB, Payload: chunk{n: 7}})

	if paneB.sawCount() != 1 {
		t.Fatalf("hidden pane B saw %d msgs, want 1 (background delivery failed)", paneB.sawCount())
	}
	// A must NOT have received B's addressed message.
	if paneA.sawCount() != 0 {
		t.Fatalf("focused pane A saw %d msgs, want 0 (no fan-out)", paneA.sawCount())
	}
}

// --- router drop-on-dead-id (R3) ---------------------------------------------

// TestRouterDropsDeadID routes a PaneMsg to an id no pane owns: it must not panic
// and must be dropped (REQUIRED test).
func TestRouterDropsDeadID(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("routing to a dead id panicked: %v", r)
		}
	}()

	cmd := m.routePaneMsg(pane.PaneID("does-not-exist"), struct{}{})
	if cmd != nil {
		t.Fatalf("dead-id route returned a non-nil cmd: %v", cmd)
	}
}

// --- teardown order (reverse spawn, once each) -------------------------------

// TestTeardownReverseOrder spawns three panes and asserts teardown Close()s them
// in reverse spawn order, exactly once each (REQUIRED test).
func TestTeardownReverseOrder(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	var log []pane.PaneID
	var logM sync.Mutex
	mk := func(kind pane.PaneKind) {
		m.RegisterFactory(kind, func(b pane.BasePane, _ *config.Config) pane.Pane {
			return &recPane{BasePane: b, closeLog: &log, closeLogM: &logM}
		})
	}
	mk("one")
	mk("two")
	mk("three")

	m.onResize(80, 24)
	_ = m.spawn("one", nil, true)
	_ = m.spawn("two", nil, false)
	_ = m.spawn("three", nil, false)

	order := m.reg.orderIDs()
	if len(order) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(order))
	}
	first, second, third := order[0], order[1], order[2]

	m.teardown()

	if len(log) != 3 {
		t.Fatalf("close log len = %d, want 3 (exactly once each)", len(log))
	}
	// Reverse spawn order: third, second, first.
	if log[0] != third || log[1] != second || log[2] != first {
		t.Fatalf("close order = %v, want reverse [%v %v %v]", log, third, second, first)
	}
	if m.reg.len() != 0 {
		t.Fatalf("registry not empty after teardown: %d", m.reg.len())
	}
}

// TestCloseIdempotent proves a pane is Closed exactly once even if remove is
// called twice (idempotency at the registry seam).
func TestCloseIdempotent(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	var p *recPane
	m.RegisterFactory("rec", func(b pane.BasePane, _ *config.Config) pane.Pane {
		p = &recPane{BasePane: b}
		return p
	})
	m.onResize(80, 24)
	_ = m.spawn("rec", nil, true)
	id := p.ID()

	_ = m.reg.remove(id)
	_ = m.reg.remove(id) // second remove is a no-op (pane already gone)
	if p.closes != 1 {
		t.Fatalf("Close called %d times, want exactly 1", p.closes)
	}
}

// --- quit-guard (K5) ---------------------------------------------------------

// TestQuitGuard proves the quit-guard seam: with a Working pane the first quit
// does not quit; with all Idle it does (REQUIRED test).
func TestQuitGuard(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	var p *recPane
	m.RegisterFactory("rec", func(b pane.BasePane, _ *config.Config) pane.Pane {
		p = &recPane{BasePane: b, status: pane.StatusWorking}
		return p
	})
	m.onResize(80, 24)
	_ = m.spawn("rec", nil, true)

	// Pane is Working → first quit arms the confirm, does NOT tear down.
	cmd := m.beginQuit()
	if cmd != nil {
		t.Fatal("first quit with a Working pane should not quit (cmd should be nil)")
	}
	if !m.quitArmed {
		t.Fatal("quit guard not armed on first quit with Working pane")
	}
	if m.quitting {
		t.Fatal("shell entered quitting state on first guarded quit")
	}

	// Second quit confirms → tears down + returns tea.Quit.
	cmd = m.beginQuit()
	if cmd == nil {
		t.Fatal("second quit should confirm and return tea.Quit")
	}
	if !m.quitting {
		t.Fatal("shell should be quitting after confirm")
	}

	// Now with an Idle-only fleet, the first quit quits immediately.
	m2, cancel2 := testModel(t)
	defer cancel2()
	m2.RegisterFactory("rec", func(b pane.BasePane, _ *config.Config) pane.Pane {
		return &recPane{BasePane: b, status: pane.StatusIdle}
	})
	m2.onResize(80, 24)
	_ = m2.spawn("rec", nil, true)
	if cmd := m2.beginQuit(); cmd == nil {
		t.Fatal("quit with all-Idle panes should quit immediately")
	}
}

// --- config hot-reload broadcast (K4) ----------------------------------------

// TestConfigReloadBroadcasts proves a successful ReloadMsg swaps the pointer and
// notifies every pane via an addressed ConfigChangedMsg.
func TestConfigReloadBroadcasts(t *testing.T) {
	m, cancel := testModel(t)
	defer cancel()

	var p *recPane
	m.RegisterFactory("rec", func(b pane.BasePane, _ *config.Config) pane.Pane {
		p = &recPane{BasePane: b}
		return p
	})
	m.onResize(80, 24)
	_ = m.spawn("rec", nil, true)
	before := p.sawCount()

	newCfg, err := config.Load(config.Opts{})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cmd := m.onReload(config.ReloadMsg{Config: newCfg})
	// onReload returns a Batch; drain it once to deliver the broadcast cmd. The
	// broadcast itself already routed ConfigChangedMsg synchronously via
	// routePaneMsg (Update of the pane ran), so the pane has already seen it.
	_ = cmd

	if p.sawCount() != before+1 {
		t.Fatalf("pane saw %d msgs after reload, want %d (ConfigChangedMsg not delivered)", p.sawCount(), before+1)
	}
	if m.cfg != newCfg {
		t.Fatal("config pointer not swapped on successful reload")
	}

	// A failed reload keeps last-good config and does NOT notify panes again.
	saw := p.sawCount()
	_ = m.onReload(config.ReloadMsg{Err: errTest})
	if p.sawCount() != saw {
		t.Fatal("failed reload should not broadcast ConfigChangedMsg")
	}
	if m.cfg != newCfg {
		t.Fatal("failed reload must keep the last-good config pointer")
	}
}

type testErr struct{}

func (testErr) Error() string { return "test error" }

var errTest = testErr{}
