package pane

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// --- Pane contract compile assertions ----------------------------------------

// testPane embeds BasePane and supplies only Init/Update/View/Close — exactly
// the shape every feature axis uses. The assertion below proves that pattern
// satisfies the canonical Pane interface.
type testPane struct {
	BasePane
	closed int
}

func (p *testPane) Init() tea.Cmd                       { return nil }
func (p *testPane) Update(tea.Msg) (Pane, tea.Cmd)      { return p, nil }
func (p *testPane) View() string                        { return "" }
func (p *testPane) Close() error                        { p.closed++; return nil }

var _ Pane = (*testPane)(nil)

func TestBasePanePromotesInterface(t *testing.T) {
	p := &testPane{BasePane: NewBase("t:1", KindLauncher, context.Background(), func(tea.Msg) {})}
	if p.ID() != "t:1" || p.Kind() != KindLauncher {
		t.Fatalf("base accessors wrong: id=%q kind=%q", p.ID(), p.Kind())
	}
	p.SetSize(80, 24)
	if w, h := p.Size(); w != 80 || h != 24 {
		t.Fatalf("SetSize/Size mismatch: %d x %d", w, h)
	}
	p.SetFocused(true)
	if !p.Focused() {
		t.Fatal("SetFocused(true) not reflected")
	}
	if m := p.Meta(); m.ID != "t:1" || m.Kind != KindLauncher || m.Status != StatusIdle {
		t.Fatalf("default Meta wrong: %+v", m)
	}
	// idempotent Close
	_ = p.Close()
	_ = p.Close()
	if p.closed != 2 {
		t.Fatalf("Close call count = %d", p.closed)
	}
}

// --- Bubble Tea v2 API proof (the W1 gate-0 empirical pin) --------------------

// miniModel exercises the real v2 root contract: Init() tea.Cmd,
// Update(tea.Msg) (tea.Model, tea.Cmd), View() tea.View (via tea.NewView), and
// the v2 key message type. If charm.land/bubbletea/v2 ever drifts from this
// shape, this file stops compiling — that is the point.
type miniModel struct{}

func (miniModel) Init() tea.Cmd { return nil }

func (m miniModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case tea.KeyPressMsg: // v2 key type (v1 was tea.KeyMsg struct)
		return m, tea.Quit
	}
	return m, nil
}

func (miniModel) View() tea.View { return tea.NewView("ok") } // v2: View returns tea.View, not string

func TestBubbleTeaV2APIShape(t *testing.T) {
	p := tea.NewProgram(miniModel{})
	if p == nil {
		t.Fatal("tea.NewProgram returned nil")
	}
	// Program.Send must match our Sender signature exactly — proves the
	// goroutine→message bridge the whole background model depends on.
	var _ Sender = p.Send
}

// --- Scrollback ring ----------------------------------------------------------

func TestScrollbackRing(t *testing.T) {
	s := NewScrollback(3)
	if s.Cap() != 3 {
		t.Fatalf("cap = %d", s.Cap())
	}
	s.Append("a")
	s.Append("b")
	if got := s.Lines(); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("partial fill wrong: %v", got)
	}
	// overflow evicts oldest, preserves chronological order
	s.AppendBatch([]string{"c", "d", "e"})
	got := s.Lines()
	if len(got) != 3 || got[0] != "c" || got[1] != "d" || got[2] != "e" {
		t.Fatalf("post-overflow order wrong: %v", got)
	}
	if s.Len() != 3 {
		t.Fatalf("len = %d", s.Len())
	}
	s.Clear()
	if s.Len() != 0 || len(s.Lines()) != 0 {
		t.Fatalf("clear did not empty ring: len=%d", s.Len())
	}
}

func TestScrollbackMinCapacity(t *testing.T) {
	s := NewScrollback(0) // clamped to 1
	s.Append("x")
	s.Append("y")
	if got := s.Lines(); len(got) != 1 || got[0] != "y" {
		t.Fatalf("min-cap ring wrong: %v", got)
	}
}
