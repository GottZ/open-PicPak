package console

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// errNoRunner guards a session with no resolved transport (the host could not be
// resolved to a sshhost Runner).
var errNoRunner = errors.New("console: no runner resolved for host")

// deviceRef is the gated device the console attaches to: an ssh host alias + the
// resolved, VID-gated remote device node. Both arrive from the discovery axis via the
// spawn args at runtime — never a shipped literal. The console attaches ONLY to a
// port discovery resolved (discovery owns the 303a:1001 gate); a missing port = no
// attach (protects the Zigbee dongle).
type deviceRef struct {
	Host    string
	Port    string
	Serial  string // display only (commonly empty — current FW rarely enriches)
	Label   string // display only
	IsLocal bool   // host runs via os/exec (local runner) → the pump needs a shell wrap
}

// Session owns one interactive sshhost run: the writer to the child's stdin (operator
// keystrokes → the remote `cat > {port}`) and the run lifecycle (start/stop/restart).
// Device bytes arrive at the pane as addressed sshhost.RunOutputMsg (K2), not through
// the Session. The run is bound to a context the Session cancels on stop, so teardown
// is uniform: ctx cancel kills the ssh child and the remote_command trap reaps the
// remote cat so it does not orphan the VID-gated port.
type Session struct {
	runner sshhost.Runner
	cc     config.Console
	dev    deviceRef
	to     pane.PaneID
	send   pane.Sender
	parent context.Context

	mu      sync.Mutex
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	inputCh chan []byte
	run     *sshhost.Run
	running bool
	closing bool // a stop WE initiated (operator/port-release/close) → exit classifies Closed
}

// newSession builds an idle session bound to the pane's context and Sender. It opens
// no transport until Start (connect = reset).
func newSession(parent context.Context, runner sshhost.Runner, cc config.Console, dev deviceRef, to pane.PaneID, send pane.Sender) *Session {
	return &Session{runner: runner, cc: cc, dev: dev, to: to, send: send, parent: parent}
}

// Running reports whether an interactive run is live.
func (s *Session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Port is the gated device node this session attaches to (the K6 release match key).
func (s *Session) Port() string { return s.dev.Port }

// Closing reports whether the last stop was operator-initiated (so a following exit
// classifies as Closed, not Sleeping).
func (s *Session) Closing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// Start opens the interactive run (connect = reset). It builds the remote pump argv
// from config (fail-loud on an unresolved {port}; never a literal node), starts the
// single sshhost transport, and spawns the stdin writer goroutine. A build/start
// error returns without a live run so the caller surfaces Error. Idempotent while a
// run is already live.
func (s *Session) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	runner := s.runner
	s.mu.Unlock()

	if runner == nil {
		return errNoRunner
	}

	argv, err := buildPumpArgv(s.cc, transportSubst{
		Host: s.dev.Host,
		Port: s.dev.Port,
		Baud: baudString(s.cc),
	}, s.dev.IsLocal)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(s.parent)
	run, stdin, err := runner.RunInteractive(runCtx, argv, s.to, s.send)
	if err != nil {
		cancel()
		return err
	}

	in := make(chan []byte, 256)
	s.mu.Lock()
	s.cancel = cancel
	s.stdin = stdin
	s.run = run
	s.running = true
	s.closing = false
	s.inputCh = in
	s.mu.Unlock()

	go writeLoop(runCtx, stdin, in)
	return nil
}

// writeLoop serializes operator input onto the child's stdin until the run context is
// cancelled. It owns the only write to stdin, so SendLine never races the writer.
func writeLoop(ctx context.Context, w io.Writer, in <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-in:
			if w != nil {
				_, _ = w.Write(b)
			}
		}
	}
}

// SendLine queues one already-encoded input chunk (line + line-ending) for the
// writer goroutine. It does not block the message loop: a full buffer hands off to a
// detached, ctx-bounded send rather than stalling Update.
func (s *Session) SendLine(b []byte) {
	s.mu.Lock()
	ch := s.inputCh
	running := s.running
	s.mu.Unlock()
	if !running || ch == nil {
		return
	}
	select {
	case ch <- b:
	default:
		go func() {
			select {
			case ch <- b:
			case <-s.parent.Done():
			}
		}()
	}
}

// stop tears the run down. operator=true marks it operator-initiated (exit → Closed).
// It cancels the run context — which kills the ssh child (CommandContext) and lets the
// remote_command trap reap the remote cat so it does not orphan the VID-gated port —
// closes stdin, and drops the writer channel. Idempotent. The buffered input channel
// is left for GC (never closed) so a racing detached SendLine cannot send on a closed
// channel; the writer goroutine exits on ctx cancel.
func (s *Session) stop(operator bool) {
	s.mu.Lock()
	if operator {
		s.closing = true
	}
	cancel := s.cancel
	stdin := s.stdin
	run := s.run
	s.cancel = nil
	s.stdin = nil
	s.inputCh = nil
	s.run = nil
	s.running = false
	s.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cancel != nil {
		cancel()
	}
	if run != nil {
		run.Cancel()
	}
}

// Stop is the operator/teardown stop (a following exit classifies as Closed). Used by
// the disconnect key, the K6 port release, and pane Close().
func (s *Session) Stop() { s.stop(true) }

// finish tears down a run that ended on its own (a genuine EOF, not operator action):
// it cleans up the goroutine + child without setting the Closing flag, so the exit
// classifies as Sleeping. Idempotent with a prior Stop (it never clears Closing).
func (s *Session) finish() { s.stop(false) }
