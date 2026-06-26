package build

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// Pipeline drives one cancelable build: preflight → codegen(×4) → docker-build →
// verify → done. It is the single long-lived goroutine the build pane launches; it
// pushes every transition and output batch to the pane via the injected Sender as an
// addressed app.PaneMsg (K2). It owns no transport — every command runs through the
// sshhost Runner (K9). One m.cancel() of the run context tears down whichever stage
// is in flight (codegen/docker/verify all run under it; docker additionally gets a
// named docker-kill fallback within cancel_kill_grace_ms).
type Pipeline struct {
	cfg      Config
	runner   sshhost.Runner
	to       pane.PaneID
	send     pane.Sender
	runSetup bool // run the setup scripts before preflight (operator confirm path)
}

// NewPipeline builds a pipeline for one run. runSetup is the confirm-driven "run the
// one-time component setup first" flag (default false: never silent).
func NewPipeline(cfg Config, runner sshhost.Runner, to pane.PaneID, send pane.Sender, runSetup bool) *Pipeline {
	return &Pipeline{cfg: cfg, runner: runner, to: to, send: send, runSetup: runSetup}
}

// runSeq mints short, unique run ids for the container name ({runid}); runtime data,
// never a code literal (air-gap).
var runSeq atomic.Uint64

func mintRunID() string {
	return strconv.FormatUint(runSeq.Add(1), 10) + "-" + strconv.FormatInt(time.Now().Unix(), 10)
}

// Run executes the pipeline to a terminal BuildDoneMsg. It is meant to run on its own
// goroutine (the pane's Init launches it). ctx bounds the whole run; cancel ends it
// at the next stage boundary or kills the in-flight stage subprocess.
func (p *Pipeline) Run(ctx context.Context) {
	var result BuildResult
	var errTail []string

	// finish emits the single terminal BuildDoneMsg.
	finish := func(stage Stage, rc int, err error) {
		result.Stage = stage
		result.RC = rc
		result.Err = err
		result.Canceled = stage == StageCanceled
		if len(errTail) > p.cfg.ErrorTailLines && p.cfg.ErrorTailLines > 0 {
			errTail = errTail[len(errTail)-p.cfg.ErrorTailLines:]
		}
		result.ErrTail = errTail
		p.emit(BuildDoneMsg{Result: result})
	}

	// canceled reports a context cancel and emits the canceled terminal.
	canceled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		finish(StageCanceled, -1, ctx.Err())
		return true
	}

	enter := func(stage Stage) time.Time {
		at := time.Now()
		p.emit(StageEnteredMsg{Stage: stage, At: at})
		return at
	}
	stageDone := func(stage Stage, rc int, since time.Time) {
		d := time.Since(since)
		result.Stages = append(result.Stages, StageTiming{Stage: stage, RC: rc, Duration: d})
		p.emit(StageDoneMsg{Stage: stage, RC: rc, Duration: d})
	}
	emitLines := func(stage Stage, lines []Line) {
		if len(lines) == 0 {
			return
		}
		for _, l := range lines {
			if l.Stream == Stderr {
				errTail = append(errTail, l.Text)
			}
		}
		p.emit(OutputLinesMsg{Stage: stage, Lines: lines})
	}
	emitFindings := func(fs []Finding) {
		for _, f := range fs {
			result.Findings = append(result.Findings, f)
			if f.Severity == SeverityBlock {
				errTail = append(errTail, f.Text)
			}
			p.emit(FindingMsg{Finding: f})
		}
	}

	if canceled() {
		return
	}

	// --- optional setup (confirm path) ------------------------------------------
	if p.runSetup {
		at := enter(StagePreflight)
		lines, rc := runSetup(ctx, p.cfg, p.runner)
		emitLines(StagePreflight, lines)
		stageDone(StagePreflight, rc, at)
		if canceled() {
			return
		}
		if rc != 0 {
			finish(StageFailed, rc, nil)
			return
		}
	}

	// --- preflight --------------------------------------------------------------
	at := enter(StagePreflight)
	findings := runPreflight(ctx, p.cfg, p.runner)
	if canceled() {
		return
	}
	emitFindings(findings)
	blocked := hasBlocking(findings)
	rc := 0
	if blocked {
		rc = 1
	}
	stageDone(StagePreflight, rc, at)
	if blocked {
		finish(StageFailed, 1, nil)
		return
	}

	// --- codegen (×4, parallel) -------------------------------------------------
	at = enter(StageCodegen)
	_, lines, cgRC := runCodegen(ctx, p.cfg, p.runner)
	if canceled() {
		return
	}
	emitLines(StageCodegen, lines)
	stageDone(StageCodegen, cgRC, at)
	if cgRC != 0 {
		finish(StageFailed, cgRC, nil)
		return
	}

	// --- docker build (the long pole; streams straight to the pane) -------------
	at = enter(StageDockerBuild)
	dRC, dErr := p.runDocker(ctx, mintRunID())
	if canceled() {
		return
	}
	stageDone(StageDockerBuild, dRC, at)
	if dRC != 0 || dErr != nil {
		finish(StageFailed, nonZero(dRC), dErr)
		return
	}

	// --- verify -----------------------------------------------------------------
	at = enter(StageVerify)
	probe := newFileProbe(ctx, p.cfg, p.runner)
	set, vfindings := Verify(p.cfg, probe)
	if canceled() {
		return
	}
	emitFindings(vfindings)
	if set == nil {
		stageDone(StageVerify, 1, at)
		finish(StageFailed, 1, nil)
		return
	}
	stageDone(StageVerify, 0, at)

	// --- done -------------------------------------------------------------------
	result.Artifacts = set
	finish(StageDone, 0, nil)
}

// runDocker runs the containerized build, streaming its output to the pane via the
// Runner (RunOutputMsg), and returns the exit code (and any transport error). On a
// cancel mid-build it fires the named docker-kill fallback so a remote orphan is
// reaped within cancel_kill_grace_ms (covers ONLY the docker stage).
func (p *Pipeline) runDocker(ctx context.Context, runid string) (int, error) {
	argv, name := dockerArgv(p.cfg, runid)
	run, err := p.runner.Run(ctx, argv, p.to, p.send)
	if run == nil {
		return -1, err
	}

	killDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// ctx is canceled: use a fresh bounded context to reap the named container.
			grace := p.cfg.CancelKillGrace
			if grace <= 0 {
				grace = 2 * time.Second
			}
			kctx, kcancel := context.WithTimeout(context.Background(), grace)
			defer kcancel()
			_, _ = p.runner.CaptureOutput(kctx, dockerKillArgv(p.cfg, name))
		case <-killDone:
		}
	}()

	<-run.Done()
	close(killDone)
	return run.ExitCode(), run.Err()
}

// nonZero coerces a 0 rc to 1 when a transport error accompanied it, so a green-rc
// transport failure never reads as success.
func nonZero(rc int) int {
	if rc == 0 {
		return 1
	}
	return rc
}

// emit addresses a payload to the build pane via the Sender (K2). A nil Sender drops
// the message (defensive; never after the pane injects it).
func (p *Pipeline) emit(payload tea.Msg) {
	if p.send == nil {
		return
	}
	p.send(app.PaneMsg{To: p.to, Payload: payload})
}
