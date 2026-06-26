package build

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// blockingRunner is a fake Runner whose CaptureOutput blocks until the run context is
// canceled. It signals (once) when first entered so the cancel test is deterministic.
type blockingRunner struct {
	entered chan struct{}
	once    sync.Once
}

func (r *blockingRunner) Argv(argv []string) []string { return argv }

func (r *blockingRunner) Run(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*sshhost.Run, error) {
	return nil, nil // the cancel test never reaches the docker stage
}

func (r *blockingRunner) RunInteractive(ctx context.Context, argv []string, to pane.PaneID, send pane.Sender) (*sshhost.Run, io.WriteCloser, error) {
	return nil, nil, nil // the build pipeline never opens an interactive run
}

func (r *blockingRunner) CaptureOutput(ctx context.Context, argv []string) (string, error) {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return "", ctx.Err()
}

func (r *blockingRunner) CloseMaster() error { return nil }

// TestPipeline_Cancel proves a cancel during an in-flight stage tears the pipeline
// down to a single Canceled terminal (no Failed/Done leakage). The runner blocks in
// preflight's first probe; canceling the run context releases it and the pipeline
// reports StageCanceled.
func TestPipeline_Cancel(t *testing.T) {
	tmp := t.TempDir()
	cfg := Config{
		Host:              "local",
		Local:             true,
		FirmwareDir:       tmp,
		FontsDir:          filepath.Join(tmp, "fonts"),
		PythonBin:         "python3",
		PythonImportProbe: "import PIL",
		DockerCmd:         "docker",
		DockerImage:       "img:test",
		ErrorTailLines:    40,
	}

	runner := &blockingRunner{entered: make(chan struct{})}
	payloads := make(chan tea.Msg, 64)
	send := func(msg tea.Msg) {
		if pm, ok := msg.(app.PaneMsg); ok {
			payloads <- pm.Payload
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	pl := NewPipeline(cfg, runner, pane.PaneID("build:1"), send, false)
	go pl.Run(ctx)

	// Wait until the pipeline is blocked inside the runner, then cancel.
	select {
	case <-runner.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never entered the runner")
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case p := <-payloads:
			if done, ok := p.(BuildDoneMsg); ok {
				if !done.Result.Canceled || done.Result.Stage != StageCanceled {
					t.Fatalf("expected a Canceled terminal, got %+v", done.Result)
				}
				if done.Result.Artifacts != nil {
					t.Fatal("a canceled build must not yield artifacts")
				}
				return
			}
		case <-deadline:
			t.Fatal("no BuildDoneMsg after cancel")
		}
	}
}
