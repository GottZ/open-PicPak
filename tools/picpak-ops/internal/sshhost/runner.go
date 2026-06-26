package sshhost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// Runner is the single transport abstraction (K9). A consumer (poll worker, build,
// flash, console) asks a Runner to start a command on a host and gets back a *Run
// whose output streams as addressed tea.Msg. Two implementations exist: sshRunner
// (system ssh via os/exec with ControlMaster reuse) and localRunner (direct
// os/exec for a host marked local). Both build argv from config (Policy=Data) and
// bind the child to a context so teardown is uniform: ctx cancel + process kill.
type Runner interface {
	// Argv assembles, but does not execute, the full command line for argv on this
	// host. Exposed for the table tests (deterministic argv assertion) and for the
	// confirm gate / discovery, which need the same assembly the *Run uses.
	Argv(argv []string) []string

	// Run starts argv on the host and returns a live *Run. Output is addressed to
	// `to` via `send`; the ring is sized to run.scrollback_lines and lines are
	// truncated to run.line_max_bytes. ctx bounds the child lifetime (cancel kills
	// it). A start failure returns the *Run with a terminal RunExitMsg already
	// emitted plus the error, so a consumer always sees a terminal message.
	Run(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, error)

	// RunInteractive is Run plus a wired stdin: it returns the *Run (stdout/stderr
	// stream as addressed RunOutputMsg/RunExitMsg exactly as Run) AND an
	// io.WriteCloser bound to the child's stdin. The bidirectional console pump
	// consumes it — operator keystrokes are written to the WriteCloser (→ the remote
	// `cat > {port}`) while device bytes arrive via RunOutputMsg. This keeps the
	// console on the single transport (K9): no consumer spawns ssh itself. A start
	// failure returns a non-nil *Run (terminal RunExitMsg already emitted), a nil
	// writer, and the error.
	RunInteractive(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, io.WriteCloser, error)

	// CaptureOutput runs argv to completion and returns the combined stdout (the
	// poll/confirm path, which parses a small bounded output rather than streaming).
	// stderr is captured into err on a non-zero exit so the worker can surface an
	// actionable hint. ctx bounds the run.
	CaptureOutput(ctx context.Context, argv []string) (string, error)

	// CloseMaster tears down any persistent transport resource for this host
	// (ssh -O exit for an ssh master socket); a no-op for a local runner. Called
	// by HostRegistry.Stop() in addition to per-run ctx cancel.
	CloseMaster() error
}

// runConfig is the subset of *Config a runner needs, snapshotted at construction
// so a hot-reload's new *Config does not mutate a runner mid-flight (K4: long jobs
// snapshot at goroutine start).
type runConfig struct {
	sshBinary   string
	baseArgs    []string
	controlPath string // ~-expanded
	runScroll   int
	lineMax     int
}

func runConfigFrom(cfg *config.Config) runConfig {
	return runConfig{
		sshBinary:   cfg.SSH.Binary,
		baseArgs:    append([]string(nil), cfg.SSH.BaseArgs...),
		controlPath: expandHome(cfg.SSH.ControlPath),
		runScroll:   cfg.Run.ScrollbackLines,
		lineMax:     cfg.Run.LineMaxBytes,
	}
}

// sshRunner runs commands on a remote host through the operator's system ssh,
// reusing one ControlMaster socket per host so per-command exec is cheap enough to
// poll at the configured cadence (§2.1). It never loads a key or implements a
// host-key callback — that all lives in the operator's ~/.ssh/config + agent.
type sshRunner struct {
	rc     runConfig
	target string // hosts[].ssh_target, an alias resolved by ~/.ssh/config
}

// newSSHRunner builds an ssh runner for a host's ssh_target.
func newSSHRunner(rc runConfig, target string) *sshRunner {
	return &sshRunner{rc: rc, target: target}
}

// Argv assembles the full ssh command line:
//
//	ssh <base_args> [-o ControlPath=<expanded>] <target> -- <argv...>
//
// base_args come verbatim from config; a ControlPath is appended as an explicit
// -o (with the leading ~ already resolved, since ssh does not ~-expand a -o
// ControlPath on all versions — §5 control_path note). The "--" stops ssh from
// interpreting the remote argv as its own options.
func (r *sshRunner) Argv(argv []string) []string {
	out := make([]string, 0, len(r.rc.baseArgs)+len(argv)+5)
	out = append(out, r.rc.sshBinary)
	out = append(out, r.rc.baseArgs...)
	if r.rc.controlPath != "" {
		out = append(out, "-o", "ControlPath="+r.rc.controlPath)
	}
	out = append(out, r.target, "--")
	out = append(out, argv...)
	return out
}

// Run starts the ssh command and returns a streaming *Run.
func (r *sshRunner) Run(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, error) {
	full := r.Argv(argv)
	return startRun(ctx, full, to, send, r.rc.runScroll, r.rc.lineMax)
}

// RunInteractive starts the ssh command with a wired stdin and returns the
// streaming *Run plus the stdin writer (the console's bidirectional pump).
func (r *sshRunner) RunInteractive(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, io.WriteCloser, error) {
	return startRunInteractive(ctx, r.Argv(argv), to, send, r.rc.runScroll, r.rc.lineMax)
}

// CaptureOutput runs the ssh command to completion and returns stdout.
func (r *sshRunner) CaptureOutput(ctx context.Context, argv []string) (string, error) {
	return captureOutput(ctx, r.Argv(argv))
}

// CloseMaster runs `ssh -O exit` against the host to tear down the ControlMaster
// socket (the namespaced control_path means a sweep reaps only ours — §9). A
// failure (no master running) is not fatal; it is returned for the caller to log.
func (r *sshRunner) CloseMaster() error {
	args := make([]string, 0, len(r.rc.baseArgs)+5)
	args = append(args, r.rc.baseArgs...)
	if r.rc.controlPath != "" {
		args = append(args, "-o", "ControlPath="+r.rc.controlPath)
	}
	args = append(args, "-O", "exit", r.target)
	cmd := exec.Command(r.rc.sshBinary, args...)
	// Best-effort: no master → non-zero exit, which is fine.
	if err := cmd.Run(); err != nil {
		// Swallow the common "no such control path" case; only an unexpected exec
		// error is worth returning. An *exec.ExitError is the no-master case.
		if _, ok := err.(*exec.ExitError); ok {
			return nil
		}
		return err
	}
	return nil
}

// startRun is the shared build-and-launch path for both runners: it builds an
// exec.CommandContext for full (full[0] is the program), wires stdout/stderr pipes,
// and launches the *Run's reader/reaper goroutines.
func startRun(ctx context.Context, full []string, to pane.PaneID, send pane.Sender, ringSize, lineMax int) (*Run, error) {
	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, full[0], full[1:]...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	r := &Run{
		id:           mintRunID(),
		cmd:          cmd,
		to:           to,
		send:         send,
		ring:         pane.NewScrollback(ringSize),
		lineMaxBytes: lineMax,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	if err := r.start(stdout, stderr); err != nil {
		return r, err
	}
	return r, nil
}

// startRunInteractive is startRun plus a wired stdin: it builds the same streaming
// *Run (stdout/stderr → addressed RunOutputMsg/RunExitMsg) and additionally returns
// an io.WriteCloser bound to the child's stdin, so the console pump can write
// operator keystrokes to the remote `cat > {port}`. The child is bound to ctx
// (CommandContext) so teardown is uniform: ctx cancel kills the child and the
// remote_command trap reaps the remote cat. On a start failure it emits the terminal
// RunExitMsg (via r.start), closes the stdin write end, and returns a nil writer.
func startRunInteractive(ctx context.Context, full []string, to pane.PaneID, send pane.Sender, ringSize, lineMax int) (*Run, io.WriteCloser, error) {
	runCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(runCtx, full[0], full[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdin.Close()
		return nil, nil, err
	}

	r := &Run{
		id:           mintRunID(),
		cmd:          cmd,
		to:           to,
		send:         send,
		ring:         pane.NewScrollback(ringSize),
		lineMaxBytes: lineMax,
		cancel:       cancel,
		done:         make(chan struct{}),
	}
	if err := r.start(stdout, stderr); err != nil {
		_ = stdin.Close()
		return r, nil, err
	}
	return r, stdin, nil
}

// captureOutput runs full to completion and returns combined stdout. On a non-zero
// exit it returns whatever stdout was captured plus an error whose message carries
// the stderr tail (so the worker can show an actionable hint, e.g. host-key
// verification vs connection refused).
func captureOutput(ctx context.Context, full []string) (string, error) {
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), &captureError{err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return string(out), nil
}

// captureError carries the exec error plus the stderr tail for an actionable hint.
type captureError struct {
	err    error
	stderr string
}

func (e *captureError) Error() string {
	if e.stderr != "" {
		return e.err.Error() + ": " + e.stderr
	}
	return e.err.Error()
}

func (e *captureError) Unwrap() error { return e.err }

// expandHome resolves a single leading ~ to $HOME (or the user home dir). ssh does
// not ~-expand a ControlPath given via -o on all versions, so the tool does it
// before passing (§5 control_path note). A path without a leading ~ is returned
// unchanged. Only the leading ~ / ~/ form is expanded (not ~user).
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if p == "~" {
		if h := homeDir(); h != "" {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h := homeDir(); h != "" {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}
