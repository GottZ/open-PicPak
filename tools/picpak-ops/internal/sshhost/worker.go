package sshhost

import (
	"context"
	"math"
	"time"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// HostWorker polls one host on its own goroutine, focus-independently (NOT a
// tea.Tick — K2/§4.1: cadence lives off the Bubble Tea thread so it keeps firing
// regardless of which pane is focused). It runs the connection-state machine
// (connecting → up → backoff → down) with exponential backoff from [reconnect],
// writes its result into the shared inventory store, and emits HostPollMsg /
// HostStateMsg to the inventory pane via the injected Sender. It never writes Model
// state directly.
type HostWorker struct {
	host    string
	runner  Runner
	disc    *discoverer
	cmd     []string // poll.command
	pollIvl time.Duration

	backoff backoffPolicy

	store  *inventoryStore
	send   pane.Sender
	target *paneTarget // inventory pane id (re-targetable: a hosts pane adopts it on spawn)

	// sem bounds concurrent host polls (poll.parallel) across all workers; nil = no cap.
	sem chan struct{}

	state  ConnState
	fails  int
	lastOK time.Time
}

// backoffPolicy captures the [reconnect] knobs.
type backoffPolicy struct {
	initial       time.Duration
	max           time.Duration
	factor        float64
	failThreshold int
}

// nextDelay returns the backoff delay for the n-th consecutive failure (n>=1):
// initial * factor^(n-1), capped at max.
func (b backoffPolicy) nextDelay(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := float64(b.initial) * math.Pow(b.factor, float64(n-1))
	if d > float64(b.max) || math.IsInf(d, 0) {
		return b.max
	}
	if d < float64(b.initial) {
		return b.initial
	}
	return time.Duration(d)
}

// run is the worker loop. It polls immediately, then on a cadence; on success it
// schedules the next poll at poll.interval, on failure at the backoff delay. It
// exits when ctx is cancelled (Stop / teardown). This is the focus-independent
// goroutine; it owns no Model state.
func (w *HostWorker) run(ctx context.Context) {
	// First poll happens promptly so the inventory paints quickly.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := w.pollOnce(ctx)
			if ctx.Err() != nil {
				return
			}
			timer.Reset(next)
		}
	}
}

// pollOnce runs one enumeration probe, updates the state machine, writes the store,
// emits the messages, and returns the delay until the next poll (cadence on
// success, backoff on failure).
func (w *HostWorker) pollOnce(ctx context.Context) time.Duration {
	// Bound concurrent polls (poll.parallel). Release on return.
	if w.sem != nil {
		select {
		case w.sem <- struct{}{}:
			defer func() { <-w.sem }()
		case <-ctx.Done():
			return w.pollIvl
		}
	}

	// Announce "connecting" only on a state edge into a poll after being down/backoff,
	// so the badge is responsive without spamming.
	out, err := w.runner.CaptureOutput(ctx, w.cmd)
	now := time.Now()

	if err != nil {
		w.fails++
		prev := w.state
		if w.fails >= w.backoff.failThreshold {
			w.state = StateDown
		} else {
			w.state = StateBackoff
		}
		if w.state != prev {
			w.emitState(w.state, err, now)
		}
		w.writeAndEmitPoll(nil, err, now)
		return w.backoff.nextDelay(w.fails)
	}

	// Success: parse + gate, reset failure count, mark up.
	devices := w.disc.parse(out)
	w.fails = 0
	w.lastOK = now
	prev := w.state
	w.state = StateUp
	if w.state != prev {
		w.emitState(StateUp, nil, now)
	}
	w.writeAndEmitPoll(devices, nil, now)
	return w.pollIvl
}

// writeAndEmitPoll records the host's new state in the shared store and emits a
// HostPollMsg to the inventory pane (K2: addressed PaneMsg via the Sender).
func (w *HostWorker) writeAndEmitPoll(devices []DiscoveredDevice, err error, at time.Time) {
	hs := HostState{
		Host:    w.host,
		State:   w.state,
		Devices: devices,
		Err:     err,
		LastOK:  w.lastOK,
		At:      at,
		Fails:   w.fails,
	}
	w.store.set(hs)
	w.emit(HostPollMsg{
		Host:    w.host,
		Devices: devices,
		Err:     err,
		State:   w.state,
		At:      at,
	})
}

// emitState emits a HostStateMsg (a state-edge badge update without a device list).
func (w *HostWorker) emitState(state ConnState, err error, at time.Time) {
	w.emit(HostStateMsg{Host: w.host, State: state, Err: err, At: at})
}

// emit addresses a payload to the inventory pane via the wm-shell Sender (K2). The
// target is read live so a hosts pane that adopts the shared registry after startup
// (SetInventoryPane) re-targets every worker's poll-push. A nil Sender drops the
// message (defensive; never after SetSender); an empty target is dropped by the
// router (R3) until a hosts pane adopts it — the inventory store still updates, so
// Snapshot() (flash's pull side) stays correct without any pane open.
func (w *HostWorker) emit(payload interface{}) {
	if w.send == nil {
		return
	}
	w.send(app.PaneMsg{To: w.target.get(), Payload: payload})
}
