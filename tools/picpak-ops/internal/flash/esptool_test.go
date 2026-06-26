package flash

import (
	"strings"
	"testing"
)

// TestDashReset proves the underscore→dash translation of esptool reset modes (the IDF
// manifest carries hard_reset/default_reset; esptool 9.x argv wants the dashed form).
func TestDashReset(t *testing.T) {
	cases := map[string]string{
		"hard_reset":    "hard-reset",
		"default_reset": "default-reset",
		"hard-reset":    "hard-reset",    // already dashed → unchanged
		"no_reset_stub": "no-reset-stub", // every underscore translated
	}
	for in, want := range cases {
		if got := dashReset(in); got != want {
			t.Errorf("dashReset(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildEsptoolArgv_Shape asserts the argv shape: the fixed write-flash verb, dashed
// reset modes (translated from underscored config), no NVS offset present, and the app
// path written LAST (offset-sorted write set).
func TestBuildEsptoolArgv_Shape(t *testing.T) {
	fc, _ := mustFlash(t)
	// Force underscored reset modes to prove translation happens in argv assembly.
	fc.Before = "default_reset"
	fc.After = "hard_reset"

	ws, err := BuildWriteSet(t.TempDir(), scrambledArtifacts())
	if err != nil {
		t.Fatalf("BuildWriteSet: %v", err)
	}
	argv := BuildEsptoolArgv(fc, "DEVNODE", "/stage", ws)
	joined := strings.Join(argv, " ")

	if !contains(argv, writeFlashVerb) {
		t.Fatalf("argv missing the fixed %q verb: %v", writeFlashVerb, argv)
	}
	for _, bad := range []string{"erase-flash", "erase_flash", "erase-region"} {
		if contains(argv, bad) {
			t.Fatalf("argv must never carry an erase verb (%q): %v", bad, argv)
		}
	}
	if !strings.Contains(joined, "--before default-reset") {
		t.Fatalf("argv --before not dashed: %s", joined)
	}
	if !strings.Contains(joined, "--after hard-reset") {
		t.Fatalf("argv --after not dashed: %s", joined)
	}
	// The protected NVS offset must never appear in the write argv.
	if strings.Contains(joined, "0x9000") {
		t.Fatalf("argv must never carry the NVS offset 0x9000: %s", joined)
	}
	// -p PORT present.
	if portFromArgv(argv) != "DEVNODE" {
		t.Fatalf("argv -p port = %q, want DEVNODE", portFromArgv(argv))
	}
	// The app bin is the LAST argv element (offset-sorted, app last).
	if last := argv[len(argv)-1]; !strings.HasSuffix(last, "picpak_fw.bin") {
		t.Fatalf("last argv element = %q, want the app bin last", last)
	}
	// The app offset (0x20000) precedes it; the bootloader offset (0x0) comes first.
	if i0, iApp := strings.Index(joined, "0x0 "), strings.Index(joined, "0x20000 "); i0 < 0 || iApp < 0 || i0 > iApp {
		t.Fatalf("offsets not ascending in argv (bootloader before app): %s", joined)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
