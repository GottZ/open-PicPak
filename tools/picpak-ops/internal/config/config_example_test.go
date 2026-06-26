package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exampleRelPath is config.example.toml relative to this package directory.
const exampleRelPath = "../../config.example.toml"

// TestExample_ParsesAndValidates is the golden test: the committed example must
// parse with strict decode and pass validation. A drift in the schema (a renamed
// key, a removed default) breaks here, not silently in production.
func TestExample_ParsesAndValidates(t *testing.T) {
	path := exampleRelPath
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("example config not found at %s: %v", path, err)
	}
	cfg, err := Load(Opts{Path: path, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("example config must load and validate: %v", err)
	}
	// Spot-check a few decoded structural facts so a silently-mangled example is
	// caught even if it happens to validate.
	if len(cfg.Build.Artifacts) != 4 {
		t.Fatalf("example should define 4 build artifacts, got %d", len(cfg.Build.Artifacts))
	}
	if len(cfg.Build.Setup) != 3 {
		t.Fatalf("example should define 3 build.setup steps, got %d", len(cfg.Build.Setup))
	}
	// FOUR codegen generators: CMake guards on screens/render/font16/policy
	// (main/CMakeLists.txt :6/:10/:14/:18). The 02-config example undercounted at
	// three (no font16); a three-generator build hard-fails CMake's requirements
	// scan, so W5 corrects the schema to the four the firmware actually needs.
	if len(cfg.Build.Codegen) != 4 {
		t.Fatalf("example should define 4 build.codegen steps, got %d", len(cfg.Build.Codegen))
	}
	if !cfg.Flash.ForbidErase {
		t.Fatal("example flash.forbid_erase must be true")
	}
}

// TestExample_PinnedPlaceholders asserts the EXACT pinned RFC-safe placeholder
// values for every leak-prone field. This is the actual air-gap guard for the
// committed example: check-airgap.sh is a denylist (a real-but-not-denylisted
// host would pass it), so the golden assertion on pinned values is what guarantees
// no real identifier shipped in the example.
func TestExample_PinnedPlaceholders(t *testing.T) {
	// Load WITHOUT resolving secrets via env so the literal placeholders survive.
	cfg, err := Load(Opts{Path: exampleRelPath, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load example: %v", err)
	}

	want := map[string]struct{ got, expect string }{
		"database.dsn":        {cfg.Database.DSN, "postgres://user:pass@db.example:5432/picpak?sslmode=require"},
		"ota.admin_api_url":   {cfg.OTA.AdminAPIURL, "https://admin.example/admin"},
		"ota.admin_api_token": {cfg.OTA.AdminAPIToken, "REPLACE_ME"},
	}
	for field, v := range want {
		if v.got != v.expect {
			t.Errorf("example %s = %q, want pinned placeholder %q", field, v.got, v.expect)
		}
	}

	if len(cfg.Hosts) != 1 {
		t.Fatalf("example should carry exactly 1 placeholder host, got %d", len(cfg.Hosts))
	}
	h := cfg.Hosts[0]
	if h.Name != "host-a" {
		t.Errorf("example hosts[0].name = %q, want %q", h.Name, "host-a")
	}
	if h.SSHTarget != "user@host.example" {
		t.Errorf("example hosts[0].ssh_target = %q, want %q", h.SSHTarget, "user@host.example")
	}

	if len(cfg.Devices) != 1 {
		t.Fatalf("example should carry exactly 1 placeholder device, got %d", len(cfg.Devices))
	}
	d := cfg.Devices[0]
	if d.Serial != "PLACEHLD" {
		t.Errorf("example devices[0].serial = %q, want %q", d.Serial, "PLACEHLD")
	}
	if d.MAC != "00:00:00:00:00:00" {
		t.Errorf("example devices[0].mac = %q, want %q", d.MAC, "00:00:00:00:00:00")
	}

	// VID:PID are the REAL public Espressif values — allowed working defaults.
	if cfg.Poll.VendorID != "303a" || cfg.Poll.ProductID != "1001" {
		t.Errorf("example VID:PID = %q:%q, want 303a:1001", cfg.Poll.VendorID, cfg.Poll.ProductID)
	}
}

// TestExample_AirgapClean runs the repo's check-airgap.sh denylist scan over the
// example as an ADDITIONAL gate (necessary, not sufficient — see PinnedPlaceholders).
// Skipped if bash or the script is unavailable.
func TestExample_AirgapClean(t *testing.T) {
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	script := filepath.Join(repoRoot, "tools", "check-airgap.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("check-airgap.sh not found at %s: %v", script, err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	exampleAbs, err := filepath.Abs(exampleRelPath)
	if err != nil {
		t.Fatalf("abs example: %v", err)
	}
	rel, err := filepath.Rel(repoRoot, exampleAbs)
	if err != nil {
		t.Fatalf("rel example: %v", err)
	}

	cmd := exec.Command("bash", script, "--files", rel)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-airgap.sh flagged the example:\n%s", string(out))
	}
	if !strings.Contains(string(out), "airgap: clean") {
		t.Fatalf("unexpected check-airgap.sh output:\n%s", string(out))
	}
}
