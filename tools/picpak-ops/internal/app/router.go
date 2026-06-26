package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// routePaneMsg delivers an addressed payload to exactly one pane's Update,
// focused or backgrounded — the single background-delivery path (K2, §4.3 step 4).
// If the target id is unknown (pane closed mid-flight), the message is DROPPED
// without panic (R3). It returns the pane-returned Cmd, already re-addressed so
// its eventual Msg comes back as PaneMsg{To:id} (the pane never sees global
// addressing). A nil/empty result means "nothing to do".
func (m *Model) routePaneMsg(to pane.PaneID, payload tea.Msg) tea.Cmd {
	p, ok := m.reg.lookup(to)
	if !ok {
		return nil // R3: drop messages addressed to dead/unknown ids
	}
	updated, cmd := p.Update(payload)
	// A pane's Update returns the (possibly new) pane value; keep the registry
	// pointing at it (panes are pointer types, but honor the contract's return).
	if updated != nil {
		m.reg.panes[to] = updated
	}
	return m.addressed(to, cmd)
}

// addressed wraps a pane-returned Cmd so its eventual Msg is re-enveloped as a
// PaneMsg addressed back to the same pane. A nil Cmd stays nil. A Cmd that returns
// nil (no follow-up) is preserved as nil so the loop does not spin on empty msgs.
func (m *Model) addressed(to pane.PaneID, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if msg == nil {
			return nil
		}
		// If the pane already produced an addressed PaneMsg (e.g. it re-emitted
		// one), respect its target instead of double-wrapping.
		if pm, ok := msg.(PaneMsg); ok {
			return pm
		}
		return PaneMsg{To: to, Payload: msg}
	}
}

// initPane runs a freshly spawned pane's Init and wraps the returned Cmd so its
// first Msg is addressed back to the pane. Init's long-lived goroutine captures
// the pane's Sender (already injected via BasePane), so its chunk messages arrive
// as addressed PaneMsgs independently of this Cmd.
func (m *Model) initPane(id pane.PaneID, p pane.Pane) tea.Cmd {
	return m.addressed(id, p.Init())
}
