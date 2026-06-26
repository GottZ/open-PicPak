package buildpane

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/build"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	return cfg
}

// newPane constructs a build pane WITHOUT calling Init (so no real build starts); the
// tests drive its Update folding directly.
func newPane(t *testing.T) *buildPane {
	t.Helper()
	base := pane.NewBase(pane.PaneID("build:1"), pane.KindBuild, context.Background(), func(tea.Msg) {})
	p, ok := New(base, mustConfig(t)).(*buildPane)
	if !ok {
		t.Fatal("New did not return *buildPane")
	}
	return p
}

// TestPane_FoldsPipelineMessages checks the stage/output/finding/terminal folding and
// the dirty marker while unfocused.
func TestPane_FoldsPipelineMessages(t *testing.T) {
	p := newPane(t)
	p.SetSize(80, 24)
	p.SetFocused(false)

	feed(t, p, build.StageEnteredMsg{Stage: build.StageCodegen, At: time.Now()})
	if p.state != build.StageCodegen {
		t.Fatalf("stage = %v, want codegen", p.state)
	}

	feed(t, p, build.OutputLinesMsg{Stage: build.StageCodegen, Lines: []build.Line{
		{Text: "wrote main/screens.h", Stream: build.Stdout},
		{Text: "boom", Stream: build.Stderr},
	}})
	if !p.dirty {
		t.Fatal("expected dirty after background output")
	}
	if got := p.ring.Len(); got < 2 {
		t.Fatalf("ring should hold the output lines, got %d", got)
	}
	if len(p.errTail) != 1 || p.errTail[0] != "boom" {
		t.Fatalf("stderr should land in errTail, got %v", p.errTail)
	}

	// Focus clears the unread marker.
	p.SetFocused(true)
	if p.dirty {
		t.Fatal("focus must clear dirty")
	}
}

// TestPane_MetaStatus verifies the Meta().Status the K5 quit-guard reads: Working
// while live, Error on a non-green terminal, Idle on a green one.
func TestPane_MetaStatus(t *testing.T) {
	p := newPane(t)

	p.live = true
	if got := p.Meta().Status; got != pane.StatusWorking {
		t.Fatalf("live status = %v, want Working", got)
	}

	// Failed terminal → Error.
	feed(t, p, build.BuildDoneMsg{Result: build.BuildResult{RC: 1, Stage: build.StageFailed}})
	if p.live {
		t.Fatal("terminal must clear live")
	}
	if got := p.Meta().Status; got != pane.StatusError {
		t.Fatalf("failed status = %v, want Error", got)
	}

	// Green terminal → Idle, Green().
	set := &build.ArtifactSet{AppSHA256: "00", Artifacts: []build.Artifact{{Name: "app"}}}
	feed(t, p, build.BuildDoneMsg{Result: build.BuildResult{RC: 0, Stage: build.StageDone, Artifacts: set}})
	if got := p.Meta().Status; got != pane.StatusIdle {
		t.Fatalf("green status = %v, want Idle", got)
	}
	if p.result == nil || !p.result.Green() {
		t.Fatal("expected a green result")
	}
}

// TestPane_CancelKey cancels a live run via the pane-local cancel binding.
func TestPane_CancelKey(t *testing.T) {
	p := newPane(t)
	canceled := false
	p.live = true
	p.cancel = func() { canceled = true }

	p.onKey(keyPress(p.cfg.CancelKey))
	if !canceled {
		t.Fatalf("cancel key %q did not cancel the run", p.cfg.CancelKey)
	}
}

// TestPane_View renders without panicking in each lifecycle state.
func TestPane_View(t *testing.T) {
	p := newPane(t)
	p.SetSize(100, 30)
	_ = p.View() // idle
	feed(t, p, build.StageEnteredMsg{Stage: build.StagePreflight, At: time.Now()})
	feed(t, p, build.BuildDoneMsg{Result: build.BuildResult{RC: 1, Stage: build.StageFailed, ErrTail: []string{"err line"}}})
	if got := p.View(); got == "" {
		t.Fatal("View returned empty on a failed terminal")
	}
}

func feed(t *testing.T, p *buildPane, msg tea.Msg) {
	t.Helper()
	p.Update(msg)
}

// keyPress builds a tea.KeyPressMsg whose String() equals key (single rune keys).
func keyPress(key string) tea.KeyPressMsg {
	r := []rune(key)
	if len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: key}
	}
	return tea.KeyPressMsg{Text: key}
}
