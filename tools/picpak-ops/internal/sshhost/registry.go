package sshhost

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// HostRegistry is the single transport + discovery owner (K9). It is constructed
// once at boot after wm-shell has the Sender (§4.5), owns one HostWorker goroutine
// per enabled host, the strict VID:PID gate, and the live *Run handles. Consumers
// (the inventory pane, and later build/flash/console) read Snapshot(), start a
// streaming command via Run(), gate a device via Confirm(), and the shell calls
// Stop() on teardown.
//
// Threading: workers run off the Bubble Tea thread and reach the UI only via the
// Sender (addressed PaneMsg). HostRegistry methods are safe to call from the
// message loop (the pane's Update / Init) and from a consumer goroutine; the
// inventory store and the run set are mutex-protected.
type HostRegistry struct {
	cfg  *config.Config
	send pane.Sender

	store *inventoryStore
	gate  *gate

	// target is the shared, re-targetable inventory pane id every worker addresses
	// its poll-push to. It is shared (one allocation) so SetInventoryPane re-targets
	// all workers at once — the registry can be built at app startup before any pane
	// exists, then a hosts pane adopts it on spawn.
	target *paneTarget

	// runnersByHost maps a host name to its Runner (ssh or local), built once at
	// construction from a snapshot of config (K4: long jobs snapshot at start).
	runnersByHost map[string]Runner

	rootCtx context.Context
	cancel  context.CancelFunc

	workers []*workerHandle

	mu      sync.Mutex
	runs    map[RunID]*Run
	stopped bool
}

type workerHandle struct {
	worker *HostWorker
	cancel context.CancelFunc
}

// paneTarget holds the inventory pane id poll-push is addressed to. It is shared by
// every worker and re-targetable at runtime (SetInventoryPane), so the registry can
// be constructed before any pane exists (the app-startup shared instance) and a
// hosts pane can adopt the live poll stream when it spawns.
type paneTarget struct {
	mu sync.RWMutex
	id pane.PaneID
}

func newPaneTarget(id pane.PaneID) *paneTarget { return &paneTarget{id: id} }

func (t *paneTarget) get() pane.PaneID {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.id
}

func (t *paneTarget) set(id pane.PaneID) {
	t.mu.Lock()
	t.id = id
	t.mu.Unlock()
}

// NewHostRegistry builds the registry from config, the injected Sender, and the
// inventory pane id messages are addressed to. It does NOT start the workers — call
// Start() once the pane exists (so the first poll's HostPollMsg has a live target).
// Hosts with enabled=false get a Disabled placeholder state and no worker.
func NewHostRegistry(cfg *config.Config, send pane.Sender, inventoryPane pane.PaneID) *HostRegistry {
	rc := runConfigFrom(cfg)

	order := make([]string, 0, len(cfg.Hosts))
	for i := range cfg.Hosts {
		order = append(order, cfg.Hosts[i].Name)
	}
	store := newInventoryStore(order)

	g := newGate(
		cfg.Poll.VendorID,
		cfg.Poll.ProductID,
		cfg.Poll.ConfirmCommand,
		cfg.Poll.ConfirmRequired,
		cfg.Poll.ConfirmCacheTTL.D(),
	)

	rootCtx, cancel := context.WithCancel(context.Background())

	target := newPaneTarget(inventoryPane)

	r := &HostRegistry{
		cfg:           cfg,
		send:          send,
		target:        target,
		store:         store,
		gate:          g,
		runnersByHost: make(map[string]Runner, len(cfg.Hosts)),
		rootCtx:       rootCtx,
		cancel:        cancel,
		runs:          make(map[RunID]*Run),
	}

	// poll.parallel shared semaphore across all workers.
	var sem chan struct{}
	if cfg.Poll.Parallel > 0 {
		sem = make(chan struct{}, cfg.Poll.Parallel)
	}

	backoff := backoffPolicy{
		initial:       cfg.Reconnect.BackoffInitial.D(),
		max:           cfg.Reconnect.BackoffMax.D(),
		factor:        cfg.Reconnect.BackoffFactor,
		failThreshold: cfg.Reconnect.FailThreshold,
	}
	if backoff.failThreshold < 1 {
		backoff.failThreshold = 1
	}

	for i := range cfg.Hosts {
		h := cfg.Hosts[i]
		runner := newRunner(rc, h)
		r.runnersByHost[h.Name] = runner

		if !h.IsEnabled() {
			store.set(HostState{Host: h.Name, State: StateDisabled})
			continue
		}

		disc, err := newDiscoverer(h.Name, cfg.Poll.MatchRegex, cfg.Poll.MACRegex)
		if err != nil {
			// A bad match_regex is config-validated upstream; defensively mark the
			// host errored rather than panicking, and skip its worker.
			store.set(HostState{Host: h.Name, State: StateDown, Err: err, At: time.Now()})
			continue
		}

		w := &HostWorker{
			host:    h.Name,
			runner:  runner,
			disc:    disc,
			cmd:     append([]string(nil), cfg.Poll.Command...),
			pollIvl: cfg.Poll.Interval.D(),
			backoff: backoff,
			store:   store,
			send:    send,
			target:  target,
			sem:     sem,
			state:   StateConnecting,
		}
		r.workers = append(r.workers, &workerHandle{worker: w})
	}

	return r
}

// Start launches each enabled host's poll worker goroutine. Idempotent-ish: calling
// it after Stop() is a no-op. Call once the inventory pane exists so the first
// HostPollMsg lands on a live pane.
func (r *HostRegistry) Start() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	for _, wh := range r.workers {
		ctx, cancel := context.WithCancel(r.rootCtx)
		wh.cancel = cancel
		go wh.worker.run(ctx)
	}
}

// SetInventoryPane re-targets every worker's poll-push to a new inventory pane id.
// The app-startup shared registry is built with an empty target; the first hosts
// pane adopts the live poll stream by calling this with its own id on spawn. flash
// never calls it — it consumes Snapshot() (pull), Confirm(), and Run() only.
func (r *HostRegistry) SetInventoryPane(id pane.PaneID) {
	r.target.set(id)
}

// Snapshot returns an immutable, lock-free view of every host's current state for
// the inventory pane's synchronous View()/first paint (§4.3 pull side). It reads the
// store the workers write, so it is correct even when no pane has adopted the poll
// target — the basis for flash's target picker with no hosts pane open.
func (r *HostRegistry) Snapshot() Inventory {
	return r.store.snapshot()
}

// Run starts a long-running streaming command on a host and returns its *Run, whose
// output is addressed to `to` via the registry's Sender (K9: build/flash/console
// consume this). An unknown host or a start failure returns an error; the *Run (if
// non-nil) has already emitted a terminal RunExitMsg. The run is tracked so Stop()
// can cancel it.
func (r *HostRegistry) Run(ctx context.Context, host string, argv []string, to pane.PaneID) (*Run, error) {
	runner, ok := r.runnersByHost[host]
	if !ok {
		return nil, errors.New("sshhost: unknown host " + host)
	}
	run, err := runner.Run(ctx, argv, to, r.send)
	if run != nil {
		r.mu.Lock()
		r.runs[run.ID()] = run
		r.mu.Unlock()
		// Reap the tracking entry when the run finishes so the map does not grow.
		go func(id RunID, done <-chan struct{}) {
			<-done
			r.mu.Lock()
			delete(r.runs, id)
			r.mu.Unlock()
		}(run.ID(), run.Done())
	}
	return run, err
}

// Confirm runs the strict VID:PID gate for a device before any open/flash (§2.2).
// Fail-closed under confirm_required: an unconfirmable or mismatched device returns
// an error and is never acted on. A positive confirm is cached per
// (host,tty,descriptor) for confirm_cache_ttl.
func (r *HostRegistry) Confirm(dev DiscoveredDevice) error {
	runner, ok := r.runnersByHost[dev.Host]
	if !ok {
		return errors.New("sshhost: unknown host " + dev.Host)
	}
	ctx := r.rootCtx
	if to := r.cfg.SSH.CommandTimeout.D(); to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}
	return r.gate.confirm(ctx, runner, dev)
}

// Stop tears down every worker and run uniformly (§9): cancel the root context
// (which cancels all worker + run children via CommandContext, killing local and
// ssh children alike), then run `ssh -O exit` per ssh host as the additional master-
// socket teardown step (a no-op for local runners). Idempotent. It waits briefly
// for in-flight runs to reap so a re-flash sees a clean device.
func (r *HostRegistry) Stop() error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	runs := make([]*Run, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run)
	}
	r.mu.Unlock()

	// Cancel the root ctx: stops every worker loop and kills every run child
	// (CommandContext) — uniform across ssh and local (§9).
	r.cancel()

	// Wait (bounded) for runs to reap so children are reaped before we exit.
	for _, run := range runs {
		select {
		case <-run.Done():
		case <-time.After(2 * time.Second):
		}
	}

	// Additional ssh-only step: exit each ControlMaster socket.
	var firstErr error
	for _, runner := range r.runnersByHost {
		if err := runner.CloseMaster(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
