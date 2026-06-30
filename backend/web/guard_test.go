package web

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// T7 — TestIngestDoesNotImportWeb pins the M5-aligned hygiene invariant (D19.2):
// the public device-facing cmd/ingest binary must never depend on this package,
// directly or transitively. The day it does, the public scratch image would
// embed the operator SPA bytes, putting them in the address space reachable
// from the public parser. Same structural mechanism as Doc 17 T6
// (cmd/ingest ↛ internal/operator).
//
// Negative probe (must turn this test red): add
//
//	_ "github.com/open-picpak/backend/web"
//
// to cmd/ingest/main.go imports and re-run; revert → green.
func TestIngestDoesNotImportWeb(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		// Compiled-test-binary-only environments lack the toolchain; CI and
		// local `go test` always have it, which is where the guard matters.
		t.Skipf("go binary not on PATH: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "github.com/open-picpak/backend/cmd/ingest")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list -deps failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("go list -deps failed: %v", err)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(dep) == "github.com/open-picpak/backend/web" {
			t.Fatal("cmd/ingest imports github.com/open-picpak/backend/web — the public binary must stay " +
				"frontend-free so the operator SPA bytes are not in the public parser's address space (D19.2)")
		}
	}
}
