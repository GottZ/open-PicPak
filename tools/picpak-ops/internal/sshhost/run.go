package sshhost

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// Run is the handle for one long-running command (a poll probe, a docker build, an
// esptool write, a ttyACM console pump — K9: this is the single transport every
// such consumer streams through). It owns the child process and two reader
// goroutines that emit RunOutputMsg per line and a single RunExitMsg on exit, all
// addressed to the borrowing pane via the shared Sender. The output also lands in a
// bounded ring (run.scrollback_lines) so a backgrounded run survives and a
// re-rendering pane can repaint from the ring without having buffered every delta.
type Run struct {
	id  RunID
	cmd *exec.Cmd

	// to is the pane the output is addressed to; send is the wm-shell bridge.
	to   pane.PaneID
	send pane.Sender

	ring         *pane.Scrollback
	lineMaxBytes int

	// cancel cancels the run's context (kills the child via CommandContext).
	cancel context.CancelFunc

	// wg waits for both pipe readers; done is closed after the process is reaped.
	wg   sync.WaitGroup
	once sync.Once
	done chan struct{}

	mu       sync.Mutex
	finished bool
	exitCode int
	exitErr  error
}

// ID returns the run's identifier.
func (r *Run) ID() RunID { return r.id }

// Ring returns the run's scrollback ring (the survives-in-background backing
// store). A borrowing pane reads it on the message loop; a reader goroutine writes
// it — Scrollback is goroutine-safe.
func (r *Run) Ring() *pane.Scrollback { return r.ring }

// Done returns a channel closed once the process has exited and both readers have
// drained. Useful for a consumer (registry.Stop) that wants to await reaping.
func (r *Run) Done() <-chan struct{} { return r.done }

// Finished reports whether the run has completed (process reaped). Safe to call
// from the message loop.
func (r *Run) Finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finished
}

// ExitCode returns the process exit code once the run has finished (the value also
// carried in RunExitMsg). It is meaningful only after Done() is closed; before that
// it returns 0. A build pipeline that drives a stage to completion (<-run.Done())
// reads it here instead of intercepting the pane-addressed RunExitMsg.
func (r *Run) ExitCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode
}

// Err returns the run's terminal exec/wait error (nil for a clean exit, even a
// non-zero one — that code lives in ExitCode). Meaningful only after Done().
func (r *Run) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitErr
}

// Cancel terminates the run: it cancels the context, which kills the child via
// exec.CommandContext; the reader goroutines drain and the reap path emits the
// single RunExitMsg. Idempotent.
func (r *Run) Cancel() { r.cancel() }

// start wires the pipes, launches the process and the reader/reaper goroutines.
// It is called by the Runner immediately after building the *Run. On a start
// failure it emits a synthetic RunExitMsg so the borrowing pane always sees a
// terminal message and is never left waiting.
func (r *Run) start(stdout, stderr io.ReadCloser) error {
	if err := r.cmd.Start(); err != nil {
		// Emit a terminal message so the consumer is not left hanging, and mark
		// the run finished so Done is closed.
		r.finish(-1, err)
		return err
	}

	r.wg.Add(2)
	go r.pump(stdout, Stdout)
	go r.pump(stderr, Stderr)

	go r.reap()
	return nil
}

// pump reads one stream line-by-line, truncates to line_max_bytes, appends to the
// ring, and emits a RunOutputMsg addressed to the borrowing pane. A pathological
// unbounded line is bounded by bufio plus the explicit truncation so it never OOMs.
func (r *Run) pump(rc io.ReadCloser, stream Stream) {
	defer r.wg.Done()
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	// Cap the token buffer so a line without a newline cannot grow without bound.
	max := r.lineMaxBytes
	if max < 1 {
		max = 1
	}
	sc.Buffer(make([]byte, 0, 4096), max)

	for sc.Scan() {
		line := sc.Text()
		if len(line) > max {
			line = line[:max]
		}
		r.ring.Append(line)
		r.emit(RunOutputMsg{RunID: r.id, Stream: stream, Line: line})
	}
	// A bufio.ErrTooLong on an over-long line is swallowed: bufio stops the
	// scanner, but we have already bounded memory; the reaper still reaps. Other
	// read errors (pipe closed on exit) are normal at teardown and not surfaced.
}

// reap waits for both readers to drain, waits the process, records the exit code,
// and emits the single terminal RunExitMsg.
func (r *Run) reap() {
	r.wg.Wait()
	err := r.cmd.Wait()
	code := exitCode(err)
	r.finish(code, normalizeWaitErr(err))
}

// finish records the terminal result once and emits RunExitMsg + closes done.
func (r *Run) finish(code int, err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.finished = true
		r.exitCode = code
		r.exitErr = err
		r.mu.Unlock()
		r.emit(RunExitMsg{RunID: r.id, Code: code, Err: err})
		close(r.done)
	})
}

// emit addresses a payload to the borrowing pane via the wm-shell Sender (K2): it
// wraps the payload in app.PaneMsg{To, Payload} so the root router delivers it to
// the one pane, focused or backgrounded. A nil Sender (defensive; never the case
// after SetSender) drops the message rather than panicking.
func (r *Run) emit(payload tea.Msg) {
	if r.send == nil {
		return
	}
	r.send(app.PaneMsg{To: r.to, Payload: payload})
}

// exitCode extracts the process exit code from a Wait error. A clean exit (nil)
// is 0; an *exec.ExitError carries the code; anything else (killed by signal,
// context cancel) is -1.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
	}
	return -1
}

// normalizeWaitErr drops the noisy *exec.ExitError (its code is already in Code) so
// RunExitMsg.Err is non-nil only for an actual exec/wait failure (start failure,
// context cancel, signal), not for a clean non-zero exit.
func normalizeWaitErr(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil // the non-zero code is carried in Code, not as an error
	}
	return err
}
