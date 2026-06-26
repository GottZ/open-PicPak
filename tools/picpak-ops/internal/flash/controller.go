package flash

import (
	"context"
	"path/filepath"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// RunHandle is the subset of *sshhost.Run the controller consumes, so a test can fake
// the transport without constructing a real ssh child. *sshhost.Run satisfies it.
type RunHandle interface {
	ID() sshhost.RunID
	Ring() *pane.Scrollback
	Done() <-chan struct{}
	ExitCode() int
	Err() error
	Cancel()
}

// Transport is the subset of *sshhost.HostRegistry the controller consumes (K9):
// Confirm for the VID:PID gate and Run for the esptool write. flash never spawns ssh
// itself — it hands argv to Run and parses the streamed output.
type Transport interface {
	Confirm(dev sshhost.DiscoveredDevice) error
	Run(ctx context.Context, host string, argv []string, to pane.PaneID) (RunHandle, error)
}

// registryTransport adapts *sshhost.HostRegistry to Transport (the return-type bridge:
// HostRegistry.Run returns *sshhost.Run; this re-types it as RunHandle, guarding the
// nil-pointer-in-interface case).
type registryTransport struct{ reg *sshhost.HostRegistry }

// NewRegistryTransport wraps the shared HostRegistry as the controller's Transport.
func NewRegistryTransport(reg *sshhost.HostRegistry) Transport { return registryTransport{reg: reg} }

func (t registryTransport) Confirm(dev sshhost.DiscoveredDevice) error { return t.reg.Confirm(dev) }

func (t registryTransport) Run(ctx context.Context, host string, argv []string, to pane.PaneID) (RunHandle, error) {
	run, err := t.reg.Run(ctx, host, argv, to)
	if run == nil {
		return nil, err
	}
	return run, err
}

// Controller orchestrates a flash run across N targets with bounded concurrency. It
// owns the per-target goroutines and emits ONLY addressed app.PaneMsg to the flash
// pane (K2) — plus the one top-level app.ReleaseDevicePortMsg (K6) before each
// device's run. It owns no transport (K9): esptool rides Transport.Run, staging rides
// the Stager (a sshhost Runner).
type Controller struct {
	baseCtx   context.Context
	flashCfg  config.Flash
	artifacts []config.BuildArtifact
	artDir    string // local build artifact dir: write-set source + overlap-size stat
	guard     NVSGuard

	transport Transport
	stagerFor func(t Target) (Stager, error)

	to   pane.PaneID
	send pane.Sender

	sem chan struct{} // global concurrency bound (flash.concurrency)

	mu        sync.Mutex
	hostLocks map[string]*sync.Mutex // per-host serialization (D3: parallel across hosts only)
	cancels   map[TargetID]context.CancelFunc
	running   bool
}

// NewController builds a controller from explicit collaborators (the seam the tests
// drive). Production wiring goes through NewControllerForRegistry.
func NewController(baseCtx context.Context, fc config.Flash, artifacts []config.BuildArtifact, artDir string,
	transport Transport, stagerFor func(Target) (Stager, error), to pane.PaneID, send pane.Sender) *Controller {
	conc := fc.Concurrency
	if conc < 1 {
		conc = 1
	}
	return &Controller{
		baseCtx:   baseCtx,
		flashCfg:  fc,
		artifacts: artifacts,
		artDir:    artDir,
		guard:     GuardFromConfig(fc),
		transport: transport,
		stagerFor: stagerFor,
		to:        to,
		send:      send,
		sem:       make(chan struct{}, conc),
		hostLocks: map[string]*sync.Mutex{},
		cancels:   map[TargetID]context.CancelFunc{},
	}
}

// NewControllerForRegistry is the production constructor: it binds the shared
// HostRegistry as the transport and a per-target Stager built from the single sshhost
// Runner (K9). artDir is the local build output dir (the scp source + the size-stat
// source). baseCtx is the flash pane's per-pane context (cancelled on Close).
func NewControllerForRegistry(baseCtx context.Context, cfg *config.Config, reg *sshhost.HostRegistry, to pane.PaneID, send pane.Sender) *Controller {
	localArtifactDir, repoPath := resolveArtifactDirs(cfg)
	stagerFor := func(t Target) (Stager, error) {
		runner, err := sshhost.RunnerFor(cfg, t.Host)
		if err != nil {
			return nil, err
		}
		local := true
		if h, ok := cfg.HostByName(t.Host); ok {
			local = h.Local
		}
		hostBuildDir := localArtifactDir
		if !local {
			hostBuildDir = resolveUnder(repoPath, cfg.Build.ArtifactDir)
		}
		return newStager(cfg.Flash, t.Host, local, hostBuildDir, runner, to, send), nil
	}
	return NewController(baseCtx, cfg.Flash, cfg.Build.Artifacts, localArtifactDir,
		NewRegistryTransport(reg), stagerFor, to, send)
}

// resolveArtifactDirs returns the local build artifact dir (absolute) + the repo path
// the build axis anchors on, reusing the same resolution rule so flash reads the same
// build output the build axis produced.
func resolveArtifactDirs(cfg *config.Config) (artifactDir, repoPath string) {
	repo := cfg.Build.RepoPath
	if abs, err := filepath.Abs(repo); err == nil {
		repo = abs
	}
	return resolveUnder(repo, cfg.Build.ArtifactDir), repo
}

// Start kicks off the run on a background goroutine and returns immediately. It emits
// FlashStartedMsg synchronously-after-kickoff so the matrix initializes its rows.
func (c *Controller) Start(targets []Target) {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	c.emit(FlashStartedMsg{Targets: append([]Target(nil), targets...)})
	go c.runAll(targets)
}

// runAll fans out the targets under the global concurrency bound + per-host
// serialization, then emits the terminal FlashBatchDoneMsg with the per-device map
// (never a single batch bool).
func (c *Controller) runAll(targets []Target) {
	var wg sync.WaitGroup
	summary := make(map[TargetID]bool, len(targets))
	var smu sync.Mutex

	for _, t := range targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			// Per-host lock first (D3: serial within a host), then the global slot, so a
			// same-host queue does not occupy a concurrency slot while it waits.
			hl := c.hostLock(t.Host)
			hl.Lock()
			defer hl.Unlock()

			c.sem <- struct{}{}
			defer func() { <-c.sem }()

			ok := c.flashOne(t)
			smu.Lock()
			summary[t.ID] = ok
			smu.Unlock()
		}(t)
	}
	wg.Wait()

	c.emit(FlashBatchDoneMsg{Summary: summary})
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
}

// flashOne runs the full per-device sequence. The order is the safety order: build +
// validate the write set FAIL-CLOSED before any host I/O, then stage, then re-confirm
// the VID:PID gate, then release the port (K6), then esptool. It returns the device's
// success.
func (c *Controller) flashOne(t Target) bool {
	ctx, cancel := c.deviceCtx(t)
	defer cancel()
	defer c.clearCancel(t.ID)

	// 1. write set + argv + validate (THE safety gate; no host I/O yet).
	c.phase(t, PhaseValidating)
	ws, err := BuildWriteSet(c.artDir, c.artifacts)
	if err != nil {
		return c.fail(t, FailValidate, err.Error(), nil)
	}
	stager, serr := c.stagerFor(t)
	if serr != nil {
		return c.fail(t, FailStage, serr.Error(), nil)
	}
	hostDir := stager.HostDir(t)
	argv := BuildEsptoolArgv(c.flashCfg, t.Port(), hostDir, ws)
	if verr := ValidateWriteSet(ws, argv, c.guard); verr != nil {
		return c.fail(t, FailValidate, verr.Error(), nil)
	}

	// 2. stage the artifacts onto the host (subdir-preserving, sha-gated).
	c.phase(t, PhaseStaging)
	if err := stager.Stage(ctx, t, ws); err != nil {
		return c.fail(t, FailStage, err.Error(), nil)
	}

	// 3. re-assert the VID:PID gate on the host (anti-Zigbee + anti-staleness).
	c.phase(t, PhaseConfirming)
	if err := c.transport.Confirm(t.Device); err != nil {
		return c.fail(t, FailGate, err.Error(), nil)
	}

	// 4. K6: ask whoever holds the device port to release it BEFORE the write.
	c.releasePort(t.Port())

	// 5. esptool write, with bounded auto-retry on a transient class.
	return c.runWithRetry(ctx, t, argv, len(ws.Entries))
}

// runWithRetry runs esptool once, auto-retrying a transient class up to retry_max when
// retry_auto is on. A validate/gate/hash failure is never retried (not transient).
func (c *Controller) runWithRetry(ctx context.Context, t Target, argv []string, numFiles int) bool {
	maxRetries := 0
	if c.flashCfg.RetryAuto {
		maxRetries = c.flashCfg.RetryMax
	}
	attempts := 0
	for {
		c.phase(t, PhaseFlashing)
		ok, class, hint, tail := c.runOnce(ctx, t, argv, numFiles)
		if ok {
			return c.done(t, tail)
		}
		if class.Retryable() && attempts < maxRetries && ctx.Err() == nil {
			attempts++
			continue
		}
		return c.fail(t, class, hint, tail)
	}
}

// runOnce performs one esptool run through the transport and computes the authoritative
// verdict from the drained ring: success iff every file hash-verified AND exit 0.
//
// TODO(on-device): real flash on a test PicPak, per-device success — the HW gate
// (K11/W7) is still pending; the unit gate proves the safety/argv/parse paths only.
func (c *Controller) runOnce(ctx context.Context, t Target, argv []string, numFiles int) (bool, FailClass, string, []string) {
	run, err := c.transport.Run(ctx, t.Host, argv, c.to)
	if run == nil {
		class, hint := classifyTransport(err)
		return false, class, hint, nil
	}
	c.emit(FlashRunStartedMsg{TargetID: t.ID, RunID: run.ID(), NumFiles: numFiles})
	<-run.Done()

	lines := run.Ring().Lines()
	tail := c.tailOf(lines)
	prog := NewProgress(numFiles)
	prog.IngestAll(lines)
	if prog.Success(run.ExitCode()) {
		return true, FailNone, "", tail
	}
	class, hint := Classify(run.ExitCode(), lines, run.Err(), prog)
	return false, class, hint, tail
}

// Cancel cancels one in-flight target's run (its ctx kills the esptool child via the
// transport). Idempotent / safe on a finished target.
func (c *Controller) Cancel(id TargetID) {
	c.mu.Lock()
	cancel := c.cancels[id]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CancelAll cancels every in-flight target (abort-all / pane Close).
func (c *Controller) CancelAll() {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.cancels))
	for _, cancel := range c.cancels {
		cancels = append(cancels, cancel)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Close cancels all work (the pane's Close path). Idempotent.
func (c *Controller) Close() { c.CancelAll() }

// Running reports whether the batch goroutine is still live (the pane's StatusWorking
// driver as a fallback; the matrix's non-terminal rows are the primary signal).
func (c *Controller) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// --- internals --------------------------------------------------------------

func (c *Controller) deviceCtx(t Target) (context.Context, context.CancelFunc) {
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if to := c.flashCfg.RunTimeout.D(); to > 0 {
		ctx, cancel = context.WithTimeout(c.baseCtx, to)
	} else {
		ctx, cancel = context.WithCancel(c.baseCtx)
	}
	c.mu.Lock()
	c.cancels[t.ID] = cancel
	c.mu.Unlock()
	return ctx, cancel
}

func (c *Controller) clearCancel(id TargetID) {
	c.mu.Lock()
	delete(c.cancels, id)
	c.mu.Unlock()
}

func (c *Controller) hostLock(host string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.hostLocks[host]; ok {
		return l
	}
	l := &sync.Mutex{}
	c.hostLocks[host] = l
	return l
}

func (c *Controller) tailOf(lines []string) []string {
	n := c.flashCfg.TailLines
	if n < 1 {
		n = 40
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return append([]string(nil), lines...)
}

func (c *Controller) phase(t Target, p Phase) { c.emit(FlashPhaseMsg{TargetID: t.ID, Phase: p}) }

func (c *Controller) fail(t Target, class FailClass, hint string, tail []string) bool {
	c.emit(FlashDeviceDoneMsg{TargetID: t.ID, OK: false, Class: class, Hint: hint, Tail: tail})
	return false
}

func (c *Controller) done(t Target, tail []string) bool {
	c.emit(FlashDeviceDoneMsg{TargetID: t.ID, OK: true, Class: FailNone, Tail: tail})
	return true
}

// releasePort emits the top-level K6 message (NOT a pane-addressed PaneMsg): the root
// broadcasts it so a console pane holding this port closes before flash writes.
func (c *Controller) releasePort(port string) {
	if c.send == nil || port == "" {
		return
	}
	c.send(app.ReleaseDevicePortMsg{Port: port})
}

// emit addresses a payload to the flash pane via the Sender (K2). A nil Sender drops
// the message (defensive; never after the pane injects it).
func (c *Controller) emit(payload tea.Msg) {
	if c.send == nil {
		return
	}
	c.send(app.PaneMsg{To: c.to, Payload: payload})
}
