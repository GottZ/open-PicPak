// Package buildpane is the wm-shell view over the build pipeline (W5 / axis 04). It
// implements the canonical pane.Pane (embedding pane.BasePane) and owns the
// long-lived pipeline goroutine: on Init it starts a build, the goroutine pushes
// stage/output/finding/terminal messages back as addressed app.PaneMsg via the
// injected Sender, and Update folds them into the ring + stage state. It reports
// Meta().Status == StatusWorking while a build is live (the K5 quit-guard seam) and
// Meta().Dirty when output accrued while unfocused. It consumes the single sshhost
// Runner (K9) and never touches a device.
package buildpane

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/build"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// tickMsg drives the elapsed-time clock while a build is live (BackgroundTick was
// removed in K1/K3; a pane that wants a clock runs its own tea.Tick). It re-arms
// only while live, so a finished pane stops ticking.
type tickMsg struct{}

// buildPane implements pane.Pane for the build axis.
type buildPane struct {
	pane.BasePane

	cfg       build.Config
	runner    sshhost.Runner
	runnerErr error // non-nil → the host could not be resolved; the pane shows it, never builds

	ring   *pane.Scrollback // all output (codegen/preflight/verify + docker), survives backgrounding
	colors viewColors

	// run state, mutated only on the message loop (Update) — no lock needed.
	live       bool
	state      build.Stage
	stageStart map[build.Stage]time.Time
	stages     []build.StageTiming
	findings   []build.Finding
	result     *build.BuildResult
	errTail    []string // stderr-weighted tail for the failure box
	offerSetup bool     // a components-missing finding fired; rerun runs setup first
	buildStart time.Time
	dirty      bool

	cancel context.CancelFunc // cancels the live run's context
}

// New is the build-pane factory (registered for pane.KindBuild in main.go). It
// resolves the typed [build] view and the Runner for build.host up front; a host
// that is neither "local" nor a [[hosts]] entry is recorded as runnerErr and the
// pane refuses to build (surfacing the reason) rather than crashing.
func New(base pane.BasePane, cfg *config.Config) pane.Pane {
	bc := build.NewConfig(cfg, "")
	runner, err := sshhost.RunnerFor(cfg, bc.Host)

	p := &buildPane{
		BasePane:   base,
		cfg:        bc,
		runner:     runner,
		runnerErr:  err,
		ring:       pane.NewScrollback(bc.PaneScrollback),
		colors:     resolveColors(cfg),
		state:      build.StageIdle,
		stageStart: map[build.Stage]time.Time{},
	}
	return p
}

// Init launches the first build (spawning the pane = starting a build) and arms the
// elapsed-clock tick. If the host could not be resolved it builds nothing and just
// renders the error.
func (p *buildPane) Init() tea.Cmd {
	if p.runnerErr != nil {
		p.ring.Append("build host error: " + p.runnerErr.Error())
		return nil
	}
	p.start(false)
	return tickCmd()
}

// start kicks off one pipeline run on a fresh context derived from the pane's
// per-pane context (so Close()/teardown cancels it). runSetup runs the one-time
// component setup before preflight (the confirm path). It resets the per-run view
// state and launches the goroutine; the goroutine reaches this pane via the Sender.
func (p *buildPane) start(runSetup bool) {
	if p.runnerErr != nil || p.live {
		return
	}
	ctx, cancel := context.WithCancel(p.Context())
	p.cancel = cancel
	p.live = true
	p.state = build.StageIdle
	p.stages = nil
	p.findings = nil
	p.result = nil
	p.errTail = nil
	p.offerSetup = false
	p.buildStart = time.Now()
	p.ring.Clear()

	pl := build.NewPipeline(p.cfg, p.runner, p.ID(), p.Sender(), runSetup)
	go pl.Run(ctx)
}

// Update folds the pipeline's addressed messages (always delivered, FG or BG — K2)
// and the docker stream (sshhost.RunOutputMsg) into the ring + stage state, and
// handles the pane-local cancel/rerun keys while focused. It never blocks and runs
// no subprocess.
func (p *buildPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {

	case tickMsg:
		if p.live {
			return p, tickCmd()
		}
		return p, nil

	case build.StageEnteredMsg:
		p.state = m.Stage
		p.stageStart[m.Stage] = m.At
		p.ring.Append("── " + m.Stage.String() + " ──")
		p.markDirty()
		return p, nil

	case build.OutputLinesMsg:
		for _, l := range m.Lines {
			p.ring.Append(l.Text)
			if l.Stream == build.Stderr {
				p.pushErrTail(l.Text)
			}
		}
		p.markDirty()
		return p, nil

	case build.StageDoneMsg:
		p.stages = append(p.stages, build.StageTiming{Stage: m.Stage, RC: m.RC, Duration: m.Duration})
		p.markDirty()
		return p, nil

	case build.FindingMsg:
		p.findings = append(p.findings, m.Finding)
		if m.Finding.Fix == "setup" {
			p.offerSetup = true
		}
		tag := "warn"
		if m.Finding.Severity == build.SeverityBlock {
			tag = "BLOCK"
			p.pushErrTail(m.Finding.Text)
		}
		p.ring.Append(tag + ": " + m.Finding.Text)
		p.markDirty()
		return p, nil

	case build.BuildDoneMsg:
		res := m.Result
		p.result = &res
		p.live = false
		p.state = res.Stage
		for _, f := range res.Findings {
			if f.Fix == "setup" {
				p.offerSetup = true
			}
		}
		p.markDirty()
		return p, nil

	case sshhost.RunOutputMsg: // docker output streams straight to the pane
		p.ring.Append(m.Line)
		if m.Stream == sshhost.Stderr {
			p.pushErrTail(m.Line)
		}
		p.markDirty()
		return p, nil

	case sshhost.RunExitMsg:
		// The pipeline drives stage advancement off run.Done(); the pane ignores the
		// addressed exit so it does not double-handle the docker stage.
		return p, nil

	case tea.KeyPressMsg:
		return p, p.onKey(m)
	}
	return p, nil
}

// onKey handles the pane-local cancel/rerun bindings (only reached while focused;
// global chrome keys are consumed by the shell first). cancel ends a live build;
// rerun restarts a finished one (running setup first if a components finding fired).
func (p *buildPane) onKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case p.cfg.CancelKey:
		if p.live && p.cancel != nil {
			p.cancel()
		}
		return nil
	case p.cfg.RerunKey:
		if !p.live {
			p.start(p.offerSetup)
			return tickCmd()
		}
	}
	return nil
}

// SetFocused clears the unread marker on focus (cosmetic; never opens/closes a
// transport, R5).
func (p *buildPane) SetFocused(focused bool) {
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
	}
}

// Meta gives the pane its live title (host + stage + elapsed) and an honest status:
// Working while a build is live (the K5 quit-guard reads this), Error on a non-green
// terminal, else Idle. Dirty drives the unread marker.
func (p *buildPane) Meta() pane.PaneMeta {
	status := pane.StatusIdle
	switch {
	case p.live:
		status = pane.StatusWorking
	case p.runnerErr != nil:
		status = pane.StatusError
	case p.result != nil && !p.result.Green():
		status = pane.StatusError
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: status,
		Dirty:  p.dirty,
	}
}

// title is "build · <host> · <state> <elapsed>"; host/elapsed are runtime data.
func (p *buildPane) title() string {
	host := p.cfg.Host
	if host == "" {
		host = "local"
	}
	state := p.state.String()
	if p.live {
		return fmt.Sprintf("build · %s · %s %s", host, state, elapsed(time.Since(p.buildStart)))
	}
	if p.result != nil {
		return fmt.Sprintf("build · %s · %s", host, p.result.Stage.String())
	}
	return "build · " + host
}

// Close cancels the live run and releases the pane. Idempotent — the pipeline
// goroutine's stages all run under the canceled context (K9 uniform teardown).
func (p *buildPane) Close() error {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.live = false
	return nil
}

// markDirty flags unseen output when the pane is not focused (the unread marker).
func (p *buildPane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}

// pushErrTail keeps a bounded stderr-weighted tail for the failure box.
func (p *buildPane) pushErrTail(line string) {
	p.errTail = append(p.errTail, line)
	max := p.cfg.ErrorTailLines
	if max < 1 {
		max = 40
	}
	if len(p.errTail) > max {
		p.errTail = p.errTail[len(p.errTail)-max:]
	}
}

// tickCmd schedules the next elapsed-clock refresh.
func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// elapsed renders a short mm:ss / hh:mm:ss elapsed string.
func elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}
