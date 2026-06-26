package ota

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// newPane builds an OTA pane through the real factory with a nil pool/repo and the given
// config, sized like a real spawn.
func newPane(t *testing.T, cfg *config.Config) *otaPane {
	t.Helper()
	base := pane.NewBase(pane.PaneID("ota:test"), pane.KindOTA, context.Background(), func(tea.Msg) {})
	p, ok := New(nil, nil, fleet.New(nil))(base, cfg).(*otaPane)
	if !ok {
		t.Fatal("factory did not return *otaPane")
	}
	p.SetSize(100, 30)
	return p
}

// TestPane_NilPool is the gate's nil-pool tolerance: with no database the pane renders
// the "no database configured" state, issues no Init Cmd, has no writer, and never
// panics on Meta/Update/View.
func TestPane_NilPool(t *testing.T) {
	cfg := &config.Config{} // zero config: nil pool, empty ota/theme
	p := newPane(t, cfg)

	if p.store != nil {
		t.Fatal("nil pool should yield a nil store")
	}
	if p.writer != nil {
		t.Fatal("zero config (no admin URL, no direct-write) should yield no writer")
	}
	if cmd := p.Init(); cmd != nil {
		t.Fatal("nil-pool Init must issue no Cmd (nothing to load)")
	}
	out := p.View()
	if !strings.Contains(out, "no database configured") {
		t.Fatalf("nil-pool View should render the no-DB state, got:\n%s", out)
	}
	// These must not panic with a nil store/writer.
	_ = p.Meta()
	if _, cmd := p.Update(tickMsg{}); cmd != nil {
		t.Fatal("nil-pool tick should issue no Cmd")
	}
	p.Update(otaStateMsg{}) // a stray addressed msg must be harmless
}

// TestPane_DirectWriteBanner proves the red direct-write banner + title marker appear
// only when the active writer is the DirectPGXWriter.
func TestPane_DirectWriteBanner(t *testing.T) {
	cfg := &config.Config{}
	p := newPane(t, cfg)
	// Force the direct-write state without a real pool (banner is render-only).
	p.directWrite = true
	p.writer = NewDirectPGXWriter(nil, 0)
	p.store = NewStore(nil, 0) // non-nil so View renders the body, not the no-DB path

	out := p.View()
	if !strings.Contains(out, "DIRECT-WRITE MODE") {
		t.Fatalf("direct-write View should render the banner, got:\n%s", out)
	}
	if !strings.Contains(p.Meta().Title, "DIRECT-WRITE") {
		t.Fatalf("direct-write title should flag it, got %q", p.Meta().Title)
	}
}

// TestPane_WriteResultBadge proves a write completed while blurred sets the pending-write
// badge (Meta().Dirty) and that focusing clears it.
func TestPane_WriteResultBadge(t *testing.T) {
	cfg := &config.Config{}
	p := newPane(t, cfg)
	p.SetFocused(false)

	p.Update(writeResultMsg{kind: "register 0.6.2", err: nil})
	if !p.Meta().Dirty {
		t.Fatal("a write completed while blurred should raise Meta().Dirty (pending-write badge)")
	}
	if !strings.Contains(p.status, "register 0.6.2 ok") {
		t.Fatalf("status should reflect the completed write, got %q", p.status)
	}
	p.SetFocused(true)
	if p.Meta().Dirty {
		t.Fatal("focusing should clear the pending-write badge")
	}
}
