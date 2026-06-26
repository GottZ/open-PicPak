package main

import (
	"context"
	"testing"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// TestBuildWiring proves the bootstrap chain compiles and runs end-to-end:
// Load→New→register→NewProgram→SetSender produces a non-nil program without an
// interactive loop. The interactive TUI cannot be driven headless, so this is the
// startup-wiring proof the gate calls for.
func TestBuildWiring(t *testing.T) {
	p, cancel, err := build(context.Background(), config.Opts{}) // defaults, no file
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer cancel()
	if p == nil {
		t.Fatal("build returned a nil program")
	}
}

// TestBuildConfigErrorPropagates proves a config load failure surfaces as an error
// (exit code 1 in main), not a silent baked default — the §6.1 air-gap posture.
func TestBuildConfigErrorPropagates(t *testing.T) {
	_, _, err := build(context.Background(), config.Opts{Path: "/nonexistent/picpak-ops.toml"})
	if err == nil {
		t.Fatal("explicit --config to a missing file should be a hard error")
	}
}
