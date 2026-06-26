package telemetry

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// newPane builds a telemetry pane through the real factory with the given (possibly
// nil) pool/cache and a config, sized like a real spawn.
func newPane(t *testing.T, cfg *config.Config) *telemetryPane {
	t.Helper()
	base := pane.NewBase(pane.PaneID("telemetry:test"), pane.KindTelemetry, context.Background(), func(tea.Msg) {})
	p, ok := New(nil, fleet.New(nil))(base, cfg).(*telemetryPane)
	if !ok {
		t.Fatal("factory did not return *telemetryPane")
	}
	p.SetSize(100, 30)
	return p
}

// TestPane_NilPool is the gate's nil-pool tolerance: with no database the pane renders
// the "no database configured" state, issues no Init Cmd, and never panics on
// Meta/Update/View.
func TestPane_NilPool(t *testing.T) {
	cfg := &config.Config{} // zero config: nil pool, empty telemetry/theme
	p := newPane(t, cfg)

	if p.repo != nil {
		t.Fatal("nil pool should yield a nil repo")
	}
	if cmd := p.Init(); cmd != nil {
		t.Fatal("nil-pool Init must issue no Cmd (nothing to poll)")
	}
	out := p.View()
	if !strings.Contains(out, "no database configured") {
		t.Fatalf("nil-pool View should render the no-DB state, got:\n%s", out)
	}
	// These must not panic with a nil repo.
	_ = p.Meta()
	if _, cmd := p.Update(tickMsg{}); cmd != nil {
		t.Fatal("nil-pool tick should issue no Cmd")
	}
	p.Update(fleetMsg{}) // a stray addressed msg must be harmless
}

// TestPane_EmptyState renders the explicit "no telemetry yet" state for a
// connected-but-empty hypertable (not a crash, not an all-n/a grid).
func TestPane_EmptyState(t *testing.T) {
	cfg := &config.Config{}
	p := newPane(t, cfg)
	out := p.renderEmpty(100)
	if !strings.Contains(out, "no telemetry yet") {
		t.Fatalf("empty hypertable should render 'no telemetry yet', got:\n%s", out)
	}
}

// TestPane_FocusRequestsPoll proves a blurred→focused transition marks a poll due on
// the next tick (the K1 SetFocused can't return a Cmd, so it sets forcePoll).
func TestPane_FocusRequestsPoll(t *testing.T) {
	cfg := &config.Config{}
	p := newPane(t, cfg)
	p.SetFocused(false)
	p.forcePoll = false
	p.SetFocused(true)
	if !p.forcePoll {
		t.Fatal("blurred→focused should set forcePoll so the next tick polls")
	}
	if p.dirty {
		t.Fatal("focus should clear the dirty marker")
	}
}
