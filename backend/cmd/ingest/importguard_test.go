package main

import (
	"os/exec"
	"strings"
	"testing"
)

// T7 (extends Doc 17 T6) + A20 T8 + A24 T12: the public ingest parser must link NEITHER the secrets
// crypto (internal/sealbox) NOR the secrets store (internal/secrets) NOR the OTA write path
// (internal/rolloutadmin) NOR the BWRY frame pack (internal/bwry). An RCE in this binary then cannot
// reach Open()/ResolveSecret(), INSERT firmware_versions / blob storage, nor synthesise a frame (the pack
// is FaaS-supervisor code, not device-parser code — D24.1); ingest only PROXIES the packed frame the
// supervisor returns and serves the pre-packed embedded unavailable.bin on outage. Env exclusion is
// policy, this import guard is the structural proof, verified by the Go toolchain, not by review (the M5
// address-space boundary). ingest MAY link internal/rollout (shared, write-free), internal/otaticket
// (ingest-only key), and internal/faasstore (the webhook token-hash reader — carries no secret material).
func TestIngest_DoesNotLinkSecretMaterial(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	banned := []string{
		"github.com/open-picpak/backend/internal/sealbox",
		"github.com/open-picpak/backend/internal/secrets",
		"github.com/open-picpak/backend/internal/rolloutadmin",
		"github.com/open-picpak/backend/internal/bwry",
	}
	for _, pkg := range banned {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == pkg {
				t.Errorf("cmd/ingest links %s — secret material is reachable from the public parser", pkg)
			}
		}
	}
}

// D22.1 — the telemetry READ package (internal/telemetry, cmd/admin-only) must not link into the public
// ingest parser. ingest keeps its OWN write INSERT (main.go) and never imports the read side. This is
// structural hygiene (the same go-toolchain-verified guard as T7), not the secret boundary — telemetry
// carries no secret — but it keeps the read/write split honest at the linker.
func TestIngest_DoesNotLinkTelemetryRead(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	const banned = "github.com/open-picpak/backend/internal/telemetry"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == banned {
			t.Errorf("cmd/ingest links %s — the telemetry read package must stay cmd/admin-only (D22.1)", banned)
		}
	}
}
