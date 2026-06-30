package main

import (
	"os/exec"
	"strings"
	"testing"
)

// T7 (extends Doc 17 T6) + A20 T8: the public ingest parser must link NEITHER the secrets crypto
// (internal/sealbox) NOR the secrets store (internal/secrets) NOR the OTA write path
// (internal/rolloutadmin). An RCE in this binary then cannot reach Open()/ResolveSecret() nor
// INSERT firmware_versions / blob storage — env exclusion is policy, this import guard is the
// structural proof, verified by the Go toolchain, not by review (the M5 address-space boundary).
// ingest MAY link internal/rollout (shared, write-free) and internal/otaticket (ingest-only key).
func TestIngest_DoesNotLinkSecretMaterial(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	banned := []string{
		"github.com/open-picpak/backend/internal/sealbox",
		"github.com/open-picpak/backend/internal/secrets",
		"github.com/open-picpak/backend/internal/rolloutadmin",
	}
	for _, pkg := range banned {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == pkg {
				t.Errorf("cmd/ingest links %s — secret material is reachable from the public parser", pkg)
			}
		}
	}
}
