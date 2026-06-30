package main

import (
	"os/exec"
	"strings"
	"testing"
)

// A20 T8 (the admin half of the M5 asymmetry): the operator process must NEVER link the OTA ticket
// key path (internal/otaticket). Minting/verifying the download ticket lives ONLY in cmd/ingest, so
// the ticket key never enters the admin address space — the mirror of cmd/ingest's guard against
// linking the write path (internal/rolloutadmin). admin MAY link internal/rollout (shared, write-free)
// and internal/rolloutadmin (it owns the write path). Verified by the Go toolchain, not by review.
func TestAdmin_DoesNotLinkTicketKey(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	const banned = "github.com/open-picpak/backend/internal/otaticket"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == banned {
			t.Errorf("cmd/admin links %s — the OTA ticket key is reachable from the operator process", banned)
		}
	}
}
