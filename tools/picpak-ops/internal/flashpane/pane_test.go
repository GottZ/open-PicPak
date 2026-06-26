package flashpane

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/flash"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// newPane builds a flash pane over a registry with no hosts (Snapshot empty). The
// matrix is driven by controller messages directly — no live transport needed.
func newPane(t *testing.T) *flashPane {
	t.Helper()
	cfg := mustConfig(t)
	reg := sshhost.NewHostRegistry(cfg, func(tea.Msg) {}, "")
	base := pane.NewBase(pane.PaneID("flash:1"), pane.KindFlash, t.Context(), func(tea.Msg) {})
	p := New(reg)(base, cfg).(*flashPane)
	p.SetSize(80, 24)
	return p
}

func target(host, tty string) flash.Target {
	return flash.NewTarget(sshhost.DiscoveredDevice{Host: host, TTY: tty})
}

// TestMatrix_PerDeviceNeverCollapses proves the running view keeps ONE row per target
// (never a single batch glyph) and that Meta().Status is Working while any row is
// non-terminal (the K5 quit-guard seam), Error once all finish with a failure.
func TestMatrix_PerDeviceNeverCollapses(t *testing.T) {
	p := newPane(t)
	_ = p.Init()

	a := target("host-a", "ACM0")
	b := target("host-b", "ACM1")

	apply := func(msg tea.Msg) { _, _ = p.Update(msg) }

	apply(flash.FlashStartedMsg{Targets: []flash.Target{a, b}})

	// Two distinct rows — the matrix did not collapse to one.
	if len(p.order) != 2 {
		t.Fatalf("matrix has %d rows, want 2 (one per device)", len(p.order))
	}
	// Both rows non-terminal → the pane reports Working (K5).
	if got := p.Meta().Status; got != pane.StatusWorking {
		t.Fatalf("status while flashing = %v, want working", got)
	}

	// Live progress for device A via its run output.
	apply(flash.FlashRunStartedMsg{TargetID: a.ID, RunID: sshhost.RunID("run-a"), NumFiles: 4})
	apply(flash.FlashPhaseMsg{TargetID: a.ID, Phase: flash.PhaseFlashing})
	apply(sshhost.RunOutputMsg{RunID: "run-a", Line: "Writing at 0x00020000... (50 %)"})
	if r := p.results[a.ID]; r == nil || r.Pct != 50 {
		t.Fatalf("device A write %% not folded live: %+v", p.results[a.ID])
	}
	// A verify advances the verified count and resets the per-file %% (next file 0%%).
	apply(sshhost.RunOutputMsg{RunID: "run-a", Line: "Hash of data verified."})
	if r := p.results[a.ID]; r == nil || r.Verified != 1 {
		t.Fatalf("device A verify not folded live: %+v", p.results[a.ID])
	}

	// A succeeds, B fails — distinct per-device terminal results.
	apply(flash.FlashDeviceDoneMsg{TargetID: a.ID, OK: true})
	// While B is still non-terminal, the pane is STILL working (one device done must
	// not mask the other — the inventory mandate).
	if got := p.Meta().Status; got != pane.StatusWorking {
		t.Fatalf("status with one device still flashing = %v, want working", got)
	}
	apply(flash.FlashDeviceDoneMsg{TargetID: b.ID, OK: false, Class: flash.FailSync, Hint: "replug"})
	apply(flash.FlashBatchDoneMsg{Summary: map[flash.TargetID]bool{a.ID: true, b.ID: false}})

	if p.results[a.ID].Phase != flash.PhaseDone {
		t.Fatalf("device A phase = %v, want done", p.results[a.ID].Phase)
	}
	if p.results[b.ID].Phase != flash.PhaseFailed || p.results[b.ID].Class != flash.FailSync {
		t.Fatalf("device B = %+v, want failed/sync-fail", p.results[b.ID])
	}
	// All terminal with a failure → Error, not Working.
	if got := p.Meta().Status; got != pane.StatusError {
		t.Fatalf("status after a failed batch = %v, want error", got)
	}

	// The view shows BOTH device rows + the per-device glyphs (no batch glyph).
	view := p.View()
	for _, want := range []string{"ACM0", "ACM1", "✓", "✗", "sync-fail"} {
		if !strings.Contains(view, want) {
			t.Errorf("matrix view missing %q:\n%s", want, view)
		}
	}
}

// TestIdlePicker_EmptyInventory proves the idle picker is honest when no VID-gated
// device is present (no host configured → empty snapshot).
func TestIdlePicker_EmptyInventory(t *testing.T) {
	p := newPane(t)
	_ = p.Init()
	if got := p.Meta().Status; got != pane.StatusIdle {
		t.Fatalf("idle picker status = %v, want idle", got)
	}
	if !strings.Contains(p.View(), "No VID-gated PicPak") {
		t.Fatalf("empty picker should say so:\n%s", p.View())
	}
	_ = p.Close()
}
