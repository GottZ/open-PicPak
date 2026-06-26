package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// mustConfig loads the compiled defaults (no file, empty env) — a valid *config.Config
// whose [build] section carries the four generators, four artifacts, and the verify
// bounds.
func mustConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	return cfg
}

// localRunnerForTest builds the real local Runner (os/exec) used by the codegen /
// preflight tests that need true exit codes.
func localRunnerForTest(t *testing.T) sshhost.Runner {
	t.Helper()
	r, err := sshhost.RunnerFor(mustConfig(t), "local")
	if err != nil {
		t.Fatalf("RunnerFor local: %v", err)
	}
	return r
}

// repoRoot returns the open-picpak checkout root (four levels above this package
// dir) so a test can point verify at the real firmware/build tree read-only.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return root
}

// haveRealBuild reports whether the real firmware/build artifacts exist; the
// real-tree verify test skips when they do not (a checkout without a built firmware).
func haveRealBuild(t *testing.T, root string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, "firmware", "build", "picpak_fw.bin"))
	return err == nil
}

// ctxBG is a non-cancelable context for the local probes (no remote subprocess).
func ctxBG() context.Context { return context.Background() }
