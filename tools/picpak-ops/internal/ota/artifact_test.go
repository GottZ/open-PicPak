package ota

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// repoRoot returns the open-picpak checkout root (four levels above this package dir)
// so the artifact scan can read the real firmware/version.txt + firmware/build tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	return root
}

// TestScanArtifact reproduces the known version (0.7.0) and the known lowercase sha256
// of the built app binary. It skips on an unbuilt checkout (no firmware/build), matching
// the build axis's real-tree tests.
func TestScanArtifact(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "firmware", "build", "picpak_fw.bin")); err != nil {
		t.Skip("firmware/build not present (unbuilt checkout); skipping artifact scan")
	}

	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	cfg.Build.RepoPath = root // point the build axis at the real tree (no shared-config mutation in prod)

	art, err := ScanArtifact(cfg)
	if err != nil {
		t.Fatalf("ScanArtifact: %v", err)
	}

	const wantVer = "0.7.0"
	const wantSHA = "36d4741b7789a4c0f25ffc2006c420bfa89415e078df04ef90c77cc5357c47d2"
	if art.Version != wantVer {
		t.Errorf("version = %q, want %q", art.Version, wantVer)
	}
	if art.SHA256 != wantSHA {
		t.Errorf("sha256 = %q, want %q", art.SHA256, wantSHA)
	}
	if !ValidSHA256(art.SHA256) {
		t.Errorf("sha256 %q is not 64 lowercase hex", art.SHA256)
	}
	if art.SizeBytes <= 0 {
		t.Errorf("size = %d, want > 0", art.SizeBytes)
	}
	t.Logf("scanned version=%s size=%d sha256=%s", art.Version, art.SizeBytes, art.SHA256)
}

// TestScanArtifact_MissingArtifactName fails closed when no build.artifacts entry
// matches ota.fw_artifact_name (a misconfiguration, not a silent empty scan).
func TestScanArtifact_MissingArtifactName(t *testing.T) {
	cfg, err := config.Load(config.Opts{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	cfg.OTA.FWArtifactName = "no-such-artifact"
	if _, err := ScanArtifact(cfg); err == nil {
		t.Fatal("expected an error when fw_artifact_name names no build artifact")
	}
}
