package app

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// launcherPane is the minimal placeholder pane W3 ships so the app always lands
// somewhere (OQ#3 default). It opens no transport, runs no goroutine, and simply
// renders the spawn bindings the operator can use to open feature panes (which
// arrive in later waves and register their factories in main.go). It embeds
// BasePane and implements only Init/Update/View/Close.
type launcherPane struct {
	pane.BasePane
	spawnKeys map[string][]string // kind → spawn binding(s), from config (no literals)
}

// NewLauncher is the launcher factory (registered for pane.KindLauncher in
// main.go). It reads the spawn keymap from config so the help text is config-
// driven, never hardcoded. It is exported so cmd/picpak-ops can register it; later
// feature waves keep their own factories in their packages.
func NewLauncher(base pane.BasePane, cfg *config.Config) pane.Pane {
	return &launcherPane{
		BasePane:  base,
		spawnKeys: cfg.Keys.Spawn,
	}
}

// Init opens nothing — the launcher is inert (no background goroutine).
func (p *launcherPane) Init() tea.Cmd { return nil }

// Update ignores everything: the launcher has no interactive state. A key reaches
// it only when focused; the global chrome keys (spawn/quit/tab) are consumed by
// the shell before they get here, so the launcher needs no key handling.
func (p *launcherPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) { return p, nil }

// View renders the available spawn bindings, sized to the last SetSize.
func (p *launcherPane) View() string {
	var b strings.Builder
	b.WriteString("picpak-ops\n\n")
	if len(p.spawnKeys) == 0 {
		b.WriteString("No feature panes registered yet.\n")
		b.WriteString("Spawn bindings appear here once feature axes register their factories.")
		return b.String()
	}
	b.WriteString("Spawn a pane:\n")
	kinds := make([]string, 0, len(p.spawnKeys))
	for k := range p.spawnKeys {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		b.WriteString("  ")
		b.WriteString(strings.Join(p.spawnKeys[k], "/"))
		b.WriteString("  ")
		b.WriteString(k)
		b.WriteString("\n")
	}
	return b.String()
}

// Meta gives the launcher a stable title (config-derivable kind name, not a
// literal identifier) and an Idle status.
func (p *launcherPane) Meta() pane.PaneMeta {
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  string(pane.KindLauncher),
		Status: pane.StatusIdle,
	}
}

// Close releases nothing (the launcher owns no resources). Idempotent.
func (p *launcherPane) Close() error { return nil }
