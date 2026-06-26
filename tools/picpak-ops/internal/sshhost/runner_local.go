package sshhost

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync/atomic"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// errEmptyArgv guards a misconfigured empty command (no program to exec).
var errEmptyArgv = errors.New("sshhost: empty argv")

// localRunner runs commands directly on this box via os/exec, with no ssh master
// (§2.1: a host marked local: true / a single-machine operator / CI). It is the
// air-gap-clean way to express "the PicPak is on this very box." Teardown is
// purely ctx cancel + process kill — there is no ControlMaster to exit, so
// CloseMaster is a no-op (the ssh -O exit step is ssh-only, §9).
type localRunner struct {
	rc runConfig
}

func newLocalRunner(rc runConfig) *localRunner {
	return &localRunner{rc: rc}
}

// Argv for a local runner is the command verbatim (no ssh wrapper, no target).
func (r *localRunner) Argv(argv []string) []string {
	return append([]string(nil), argv...)
}

// Run starts argv directly and returns a streaming *Run.
func (r *localRunner) Run(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, error) {
	if len(argv) == 0 {
		return nil, errEmptyArgv
	}
	return startRun(ctx, r.Argv(argv), to, send, r.rc.runScroll, r.rc.lineMax)
}

// RunInteractive starts argv directly with a wired stdin and returns the streaming
// *Run plus the stdin writer. A local runner execs argv[0] directly, so the console
// passes a shell-wrapped pump (sh -c …) for the local case (transport.go).
func (r *localRunner) RunInteractive(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*Run, io.WriteCloser, error) {
	if len(argv) == 0 {
		return nil, nil, errEmptyArgv
	}
	return startRunInteractive(ctx, r.Argv(argv), to, send, r.rc.runScroll, r.rc.lineMax)
}

// CaptureOutput runs argv locally to completion and returns stdout.
func (r *localRunner) CaptureOutput(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errEmptyArgv
	}
	return captureOutput(ctx, r.Argv(argv))
}

// CloseMaster is a no-op for a local runner (no ssh master to exit, §9).
func (r *localRunner) CloseMaster() error { return nil }

// runSeq mints monotonic, unique RunIDs (runtime data, never a code literal —
// air-gap). It is atomic so concurrent Runs (multiple hosts) never collide.
var runSeq atomic.Uint64

func mintRunID() RunID {
	n := runSeq.Add(1)
	return RunID("run-" + strconv.FormatUint(n, 10))
}

// newRunner picks the runner kind for a host: local → localRunner, else sshRunner
// against its ssh_target.
func newRunner(rc runConfig, h config.Host) Runner {
	if h.Local {
		return newLocalRunner(rc)
	}
	return newSSHRunner(rc, h.SSHTarget)
}

// RunnerFor builds a one-off Runner for an arbitrary build host id (K9: the build
// axis consumes the single transport rather than introducing its own exec package).
// hostName is build.host: the sentinel "local" (or "") yields a localRunner; any
// other value must name a [[hosts]] entry, whose ssh_target/local fields pick the
// runner kind. The Runner snapshots the [ssh]/[run] config at construction, so a
// later hot-reload does not mutate an in-flight build (K4). Unlike HostRegistry, this
// is unmanaged: the caller owns the returned Runner's lifetime (CloseMaster on
// teardown for an ssh host; a no-op for local).
func RunnerFor(cfg *config.Config, hostName string) (Runner, error) {
	rc := runConfigFrom(cfg)
	if hostName == "" || hostName == "local" {
		return newLocalRunner(rc), nil
	}
	h, ok := cfg.HostByName(hostName)
	if !ok {
		return nil, errors.New("sshhost: build host " + hostName + " is not a [[hosts]] entry")
	}
	return newRunner(rc, *h), nil
}
