package layout

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// fakePane is a minimal Pane for layout tests: it carries a fixed meta so the tab
// bar renders deterministic labels, and a body string.
type fakePane struct {
	id    pane.PaneID
	title string
	body  string
}

func (f *fakePane) Init() tea.Cmd                       { return nil }
func (f *fakePane) Update(tea.Msg) (pane.Pane, tea.Cmd) { return f, nil }
func (f *fakePane) View() string                        { return f.body }
func (f *fakePane) SetFocused(bool)                     {}
func (f *fakePane) SetSize(int, int)                    {}
func (f *fakePane) Close() error                        { return nil }
func (f *fakePane) Meta() pane.PaneMeta {
	return pane.PaneMeta{ID: f.id, Kind: pane.KindLauncher, Title: f.title, Status: pane.StatusIdle}
}

func testTheme() Theme {
	// Colors left zero (no styling) — the hit-test is geometry-only, color-free.
	return Theme{
		GlyphActive:  "A",
		GlyphWorking: "W",
		GlyphQuiet:   "Q",
		GlyphIdle:    "I",
		GlyphError:   "E",
		DirtyMarker:  "*",
	}
}

// TestHitTestBodyAndTab proves the single hit-test (R4) maps a body click to the
// focused pane + pane-local coords, and a tab-bar click to the clicked tab's id,
// under a known layout (REQUIRED test).
func TestHitTestBodyAndTab(t *testing.T) {
	tabs := NewTabs()
	pa := &fakePane{id: "a", title: "alpha", body: "body-a"}
	pb := &fakePane{id: "b", title: "beta", body: "body-b"}
	tabs.Add(pa.id, true)  // focused
	tabs.Add(pb.id, false) // background

	m := New(tabs, testTheme(), Options{TabBar: "top", StatusBar: true})
	const W, H = 40, 20
	m.SetSize(W, H)

	panes := map[pane.PaneID]pane.Pane{pa.id: pa, pb.id: pb}
	m.Refresh(panes) // align tab spans with live meta

	// --- body click ---
	// Tab bar is row 0; body border starts at row 1; interior starts at row 2,
	// col 1. A click at interior (col 1, row 2) maps to local (0,0) of pane "a".
	hit := m.HitTest(1, 2)
	if hit.Zone != ZoneBody {
		t.Fatalf("interior click zone = %v, want ZoneBody", hit.Zone)
	}
	if hit.ID != pa.id {
		t.Fatalf("body click id = %q, want %q (focused pane)", hit.ID, pa.id)
	}
	if hit.LocalX != 0 || hit.LocalY != 0 {
		t.Fatalf("body-local coords = (%d,%d), want (0,0)", hit.LocalX, hit.LocalY)
	}
	// A click two cells right and one down maps to local (2,1).
	hit = m.HitTest(3, 3)
	if hit.Zone != ZoneBody || hit.LocalX != 2 || hit.LocalY != 1 {
		t.Fatalf("interior click (3,3) → zone=%v local=(%d,%d), want ZoneBody (2,1)", hit.Zone, hit.LocalX, hit.LocalY)
	}

	// --- tab click ---
	// Tab spans are contiguous from col 0 on row 0. The first label belongs to
	// "a"; a click inside the SECOND span must resolve to "b".
	spanA := m.rects.tabSpans[0]
	spanB := m.rects.tabSpans[1]
	if got := m.HitTest(spanA.start, 0); got.Zone != ZoneTab || got.ID != pa.id {
		t.Fatalf("tab click in span A → %+v, want ZoneTab id=a", got)
	}
	if got := m.HitTest(spanB.start, 0); got.Zone != ZoneTab || got.ID != pb.id {
		t.Fatalf("tab click in span B → %+v, want ZoneTab id=b", got)
	}

	// --- out of range ---
	if got := m.HitTest(-1, 0); got.Zone != ZoneNone {
		t.Fatalf("negative x → %+v, want ZoneNone", got)
	}
	if got := m.HitTest(W, 0); got.Zone != ZoneNone {
		t.Fatalf("x==width → %+v, want ZoneNone", got)
	}
}

func TestTabsNavigation(t *testing.T) {
	tabs := NewTabs()
	tabs.Add("a", true)
	tabs.Add("b", false)
	tabs.Add("c", false)
	if id, _ := tabs.Focused(); id != "a" {
		t.Fatalf("initial focus = %q, want a", id)
	}
	tabs.Next()
	if id, _ := tabs.Focused(); id != "b" {
		t.Fatalf("after Next focus = %q, want b", id)
	}
	tabs.Prev()
	tabs.Prev() // wrap to c
	if id, _ := tabs.Focused(); id != "c" {
		t.Fatalf("after two Prev focus = %q, want c (wrap)", id)
	}
	// Remove focused-after element; focus stays valid.
	tabs.Remove("c")
	if id, _ := tabs.Focused(); id != "b" {
		t.Fatalf("after Remove(c) focus = %q, want b", id)
	}
	tabs.Remove("a")
	tabs.Remove("b")
	if _, ok := tabs.Focused(); ok {
		t.Fatal("empty tabs should report no focus")
	}
}
