package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestVerify_RealArtifacts is the build-correctness evidence: point verify at the
// real open-picpak firmware/build tree (read-only) and prove it accepts the four
// artifacts, parses the real flasher_args.json, cross-checks every offset→file
// mapping, and computes a 64-char LOWERCASE sha256 of picpak_fw.bin that matches an
// independent hash of the same file.
func TestVerify_RealArtifacts(t *testing.T) {
	root := repoRoot(t)
	if !haveRealBuild(t, root) {
		t.Skip("firmware/build not present (unbuilt checkout)")
	}

	cfg := NewConfig(mustConfig(t), root)
	set, findings := Verify(cfg, localFileProbe{})

	for _, f := range findings {
		if f.Severity == SeverityBlock {
			t.Fatalf("unexpected blocking finding against real artifacts: %s", f.Text)
		}
	}
	if set == nil {
		t.Fatal("verify returned nil ArtifactSet for a real, valid build")
	}
	if len(set.Artifacts) != 4 {
		t.Fatalf("expected 4 verified artifacts, got %d", len(set.Artifacts))
	}

	// The app sha must be 64 lowercase hex and match an independent hash.
	if !lowerHex64.MatchString(set.AppSHA256) {
		t.Fatalf("app sha256 %q is not 64 lowercase hex", set.AppSHA256)
	}
	want := independentSHA(t, cfg.AppPath)
	if set.AppSHA256 != want {
		t.Fatalf("app sha256 mismatch: verify=%s independent=%s", set.AppSHA256, want)
	}
	if set.Version != "0.6.2" {
		t.Errorf("expected firmware version 0.6.2, got %q", set.Version)
	}

	// Offset cross-check evidence: every config offset resolves to the same file the
	// IDF flasher_args.json maps it to.
	raw, err := os.ReadFile(cfg.FlasherArgsPath)
	if err != nil {
		t.Fatalf("read flasher_args.json: %v", err)
	}
	var fa flasherArgs
	if err := json.Unmarshal(raw, &fa); err != nil {
		t.Fatalf("parse flasher_args.json: %v", err)
	}
	t.Logf("verify accepted 4 artifacts; app sha256 = %s", set.AppSHA256)
	for _, a := range set.Artifacts {
		got := fa.FlashFiles[a.Offset]
		t.Logf("offset %-8s → %-40s (config %q, %d B) match=%v",
			a.Offset, got, a.File, a.Size, normalizeBuildPath(got) == normalizeBuildPath(a.File))
		if normalizeBuildPath(got) != normalizeBuildPath(a.File) {
			t.Fatalf("offset %s cross-check failed: flasher_args=%q config=%q", a.Offset, got, a.File)
		}
	}
}

// TestVerify_Positive_Synthetic is a controlled green control for the negative tests:
// a hand-built artifact tree + flasher_args.json verifies clean.
func TestVerify_Positive_Synthetic(t *testing.T) {
	cfg := syntheticFixture(t, fixtureSizes{boot: 21152, part: 3072, ota: 8192, app: 1_403_728})
	set, findings := Verify(cfg, localFileProbe{})
	for _, f := range findings {
		if f.Severity == SeverityBlock {
			t.Fatalf("unexpected block on a valid synthetic build: %s", f.Text)
		}
	}
	if set == nil || len(set.Artifacts) != 4 || !lowerHex64.MatchString(set.AppSHA256) {
		t.Fatalf("synthetic build should verify clean: set=%v", set)
	}
}

// TestVerify_MissingArtifact: a removed artifact is a blocking finding, not a pass.
func TestVerify_MissingArtifact(t *testing.T) {
	cfg := syntheticFixture(t, fixtureSizes{boot: 21152, part: 3072, ota: 8192, app: 1_403_728})
	// Remove the app file.
	if err := os.Remove(cfg.AppPath); err != nil {
		t.Fatalf("remove app: %v", err)
	}
	set, findings := Verify(cfg, localFileProbe{})
	if set != nil {
		t.Fatal("verify must fail (nil set) when an artifact is missing")
	}
	if !findingContains(findings, SeverityBlock, "missing") {
		t.Fatalf("expected a missing-artifact block, got %+v", findings)
	}
}

// TestVerify_OffsetMismatch: a config offset that maps to a different file than
// flasher_args.json is a blocking finding.
func TestVerify_OffsetMismatch(t *testing.T) {
	cfg := syntheticFixture(t, fixtureSizes{boot: 21152, part: 3072, ota: 8192, app: 1_403_728})
	// Corrupt the app artifact's offset so it no longer matches flasher_args.json's
	// 0x20000 → picpak_fw.bin mapping (point it at the partition-table offset).
	for i := range cfg.Artifacts {
		if cfg.Artifacts[i].Name == "app" {
			cfg.Artifacts[i].Offset = "0x8000"
		}
	}
	set, findings := Verify(cfg, localFileProbe{})
	if set != nil {
		t.Fatal("verify must fail on an offset mismatch")
	}
	if !findingContains(findings, SeverityBlock, "config expects") {
		t.Fatalf("expected an offset-mismatch block, got %+v", findings)
	}
}

// TestVerify_OversizeApp: an app that does not fit ota_0 is a blocking finding.
func TestVerify_OversizeApp(t *testing.T) {
	cfg := syntheticFixture(t, fixtureSizes{boot: 21152, part: 3072, ota: 8192, app: 1_403_728})
	cfg.OTASlotSize = 1000 // force the 1.4 MB app to overflow the slot bound
	set, findings := Verify(cfg, localFileProbe{})
	if set != nil {
		t.Fatal("verify must fail when the app overflows ota_0")
	}
	if !findingContains(findings, SeverityBlock, "does not fit the ota_0 slot") {
		t.Fatalf("expected an oversize-app block, got %+v", findings)
	}
}

// --- fixture helpers ---

type fixtureSizes struct{ boot, part, ota, app int64 }

// syntheticFixture writes a controlled artifact tree + flasher_args.json under a temp
// dir and returns a Config pointing at it, so the negative tests run hermetically.
func syntheticFixture(t *testing.T, sz fixtureSizes) Config {
	t.Helper()
	root := t.TempDir()
	buildDir := filepath.Join(root, "firmware", "build")
	mustMkdir(t, filepath.Join(buildDir, "bootloader"))
	mustMkdir(t, filepath.Join(buildDir, "partition_table"))

	writeN(t, filepath.Join(buildDir, "bootloader", "bootloader.bin"), sz.boot)
	writeN(t, filepath.Join(buildDir, "partition_table", "partition-table.bin"), sz.part)
	writeN(t, filepath.Join(buildDir, "ota_data_initial.bin"), sz.ota)
	writeN(t, filepath.Join(buildDir, "picpak_fw.bin"), sz.app)

	fa := map[string]any{
		"flash_files": map[string]string{
			"0x0":     "bootloader/bootloader.bin",
			"0x8000":  "partition_table/partition-table.bin",
			"0x10000": "ota_data_initial.bin",
			"0x20000": "picpak_fw.bin",
		},
		"extra_esptool_args": map[string]any{"chip": "esp32c3"},
	}
	raw, _ := json.Marshal(fa)
	if err := os.WriteFile(filepath.Join(buildDir, "flasher_args.json"), raw, 0o644); err != nil {
		t.Fatalf("write flasher_args: %v", err)
	}
	// A version.txt so set.Version is populated.
	mustMkdir(t, filepath.Join(root, "firmware"))
	_ = os.WriteFile(filepath.Join(root, "firmware", "version.txt"), []byte("0.6.2\n"), 0o644)

	return NewConfig(mustConfig(t), root)
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

// writeN writes n bytes (deterministic content) to path.
func writeN(t *testing.T, path string, n int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = byte(i)
	}
	for n > 0 {
		w := int64(len(buf))
		if w > n {
			w = n
		}
		if _, err := f.Write(buf[:w]); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		n -= w
	}
}

func independentSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func findingContains(fs []Finding, sev Severity, sub string) bool {
	for _, f := range fs {
		if f.Severity == sev && containsFold(f.Text, sub) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexFold(s, sub) >= 0)
}

func indexFold(s, sub string) int {
	// simple case-sensitive substring index (findings are produced lowercase-stable)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
