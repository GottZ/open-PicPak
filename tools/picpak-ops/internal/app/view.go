package app

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// View renders the root into a tea.View (v2 declarative model). It sets AltScreen,
// MouseMode and ReportFocus from ui.* config — NOT as NewProgram options (§4.5) —
// and delegates the content composition to the layout engine. It does NOT set
// View.OnMouse: all mouse events flow through the Update path so the single layout
// hit-test is the only consumer (header decision; double-consume guard).
func (m *Model) View() tea.View {
	// Align tab spans with live pane meta before composing so the rendered tab bar
	// and the hit-test agree on column math.
	m.layout.Refresh(m.reg.all())

	content := m.layout.Compose(m.reg.all(), m.statusLine())
	if m.help.visible {
		content = m.overlayHelp(content)
	}
	if m.fatal != nil {
		content = m.overlayFatal(content)
	}

	v := tea.NewView(content)
	v.AltScreen = m.cfg.UI.Altscreen
	v.MouseMode = mouseMode(m.cfg.UI.Mouse)
	v.ReportFocus = m.cfg.UI.ReportFocus
	return v
}

// mouseMode maps the validated ui.mouse tri-state to the v2 MouseMode. Config
// validation guarantees the value is one of none|cell|all.
func mouseMode(mode string) tea.MouseMode {
	switch mode {
	case "all":
		return tea.MouseModeAllMotion
	case "none":
		return tea.MouseModeNone
	default: // "cell"
		return tea.MouseModeCellMotion
	}
}

// statusLine builds the shell status text: the explicit status plus a pending
// chord-prefix hint when one is armed.
func (m *Model) statusLine() string {
	s := m.status
	if pp := m.keys.PendingPrefix(); pp != "" {
		if s != "" {
			s += "  "
		}
		s += "[" + pp + "-]"
	}
	return s
}

// quitConfirmPrompt is the K5 confirm text shown when a quit is armed while panes
// are Working. It names the count so the operator knows what is in flight.
func (m *Model) quitConfirmPrompt() string {
	n := 0
	for _, p := range m.reg.all() {
		if p.Meta().Status == pane.StatusWorking {
			n++
		}
	}
	return fmt.Sprintf("%d pane(s) busy — press quit again to quit anyway", n)
}

// overlayHelp appends the global keymap help under the content (minimal overlay;
// a richer modal is a later refinement).
func (m *Model) overlayHelp(content string) string {
	bindings := m.keys.HelpBindings()
	if len(bindings) == 0 {
		return content
	}
	var help string
	for _, b := range bindings {
		h := b.Help()
		help += fmt.Sprintf("  %s: %s", h.Key, h.Desc)
	}
	return content + "\n" + help
}

// overlayFatal appends the last fatal error (the app survives; §4.3 FatalMsg).
func (m *Model) overlayFatal(content string) string {
	return content + "\n" + "error: " + m.fatal.Error()
}
