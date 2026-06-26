// Package layout is the wm-shell chrome engine: the tab bar, the active pane
// region, the status line, and the single mouse hit-test. It is tabs-first
// (Masterplan OQ#2 default): exactly one pane is visible at a time, switched by
// an ordered tab list; splits are a later, optional refinement. The package holds
// NO color or string literal — every glyph/label is runtime-derived and every
// color comes from the resolved config theme.
package layout

import "github.com/open-picpak/picpak-ops/internal/pane"

// Tabs is the ordered tab model: one tab per active pane, in spawn order, with a
// focus pointer. Focus is a layout concept (wm-shell §4.2), so it lives here, not
// in the registry. The registry holds all panes (including hidden ones); Tabs
// decides which single one is visible/focused.
type Tabs struct {
	order   []pane.PaneID // spawn order; index is the alt+N focus number (1-based)
	focused int           // index into order; -1 when empty
}

// NewTabs returns an empty tab model (focus pointer at -1).
func NewTabs() *Tabs { return &Tabs{focused: -1} }

// Len returns the number of tabs.
func (t *Tabs) Len() int { return len(t.order) }

// Order returns the tab order (read-only; callers must not mutate).
func (t *Tabs) Order() []pane.PaneID { return t.order }

// Add appends a pane as a new last tab. If focus is true (or this is the first
// tab) the new tab becomes focused.
func (t *Tabs) Add(id pane.PaneID, focus bool) {
	t.order = append(t.order, id)
	if focus || t.focused < 0 {
		t.focused = len(t.order) - 1
	}
}

// Remove drops a tab by id. Focus shifts to the previous tab (or the new last
// tab), staying valid; -1 when no tabs remain.
func (t *Tabs) Remove(id pane.PaneID) {
	idx := t.indexOf(id)
	if idx < 0 {
		return
	}
	t.order = append(t.order[:idx], t.order[idx+1:]...)
	switch {
	case len(t.order) == 0:
		t.focused = -1
	case t.focused > idx:
		t.focused--
	case t.focused == idx && t.focused >= len(t.order):
		t.focused = len(t.order) - 1
	}
}

// Focused returns the focused pane id and true, or ("", false) when empty.
func (t *Tabs) Focused() (pane.PaneID, bool) {
	if t.focused < 0 || t.focused >= len(t.order) {
		return "", false
	}
	return t.order[t.focused], true
}

// FocusedIndex returns the focused tab index, or -1 when empty.
func (t *Tabs) FocusedIndex() int { return t.focused }

// Focus sets focus to a specific pane id; no-op if the id is unknown.
func (t *Tabs) Focus(id pane.PaneID) bool {
	idx := t.indexOf(id)
	if idx < 0 {
		return false
	}
	t.focused = idx
	return true
}

// FocusIndex sets focus to a 0-based tab index; no-op if out of range.
func (t *Tabs) FocusIndex(idx int) bool {
	if idx < 0 || idx >= len(t.order) {
		return false
	}
	t.focused = idx
	return true
}

// Next moves focus to the next tab, wrapping. No-op when empty.
func (t *Tabs) Next() {
	if len(t.order) == 0 {
		return
	}
	t.focused = (t.focused + 1) % len(t.order)
}

// Prev moves focus to the previous tab, wrapping. No-op when empty.
func (t *Tabs) Prev() {
	if len(t.order) == 0 {
		return
	}
	t.focused = (t.focused - 1 + len(t.order)) % len(t.order)
}

func (t *Tabs) indexOf(id pane.PaneID) int {
	for i, p := range t.order {
		if p == id {
			return i
		}
	}
	return -1
}
