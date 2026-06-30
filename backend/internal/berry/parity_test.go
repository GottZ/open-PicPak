package berry

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// T6 — manifest↔firmware parity (bidirectional structural drift guard, D23.2).
//
// Go cannot compile Berry, so we cannot LINK the device surface the way the Go-linked guards elsewhere
// do. Instead the test regexes the be_regfunc(vm, "<name>", …) registration sites across the three C2
// surface files and asserts SET-EQUALITY with the manifest's non-forbidden surface, in BOTH directions:
// a manifest name with no registration → red; a registered name with no manifest entry (e.g. a freshly
// added dev_* getter) → red. The manifest is the declaration; the be_regfunc calls are the truth.
//
// These three files are exactly the four surfaces berry_c2 registers (cmd.c:124-132 →
// cmd_register/store_register/rtc_register/dev_register). The net (netberry.c) and fb (fb.c) drawing
// surfaces are deliberately NOT here — they are the forbidden class, registered in other phases.
var c2SurfaceFiles = []string{
	"../../../firmware/main/cmd.c",   // cmd_register: command/query/intent
	"../../../firmware/main/store.c", // store_register + rtc_register
	"../../../firmware/main/dev.c",   // dev_register (read-only)
}

// reRegfunc matches be_regfunc(vm, "<name>", …) and captures the Berry-visible name.
var reRegfunc = regexp.MustCompile(`be_regfunc\s*\(\s*vm\s*,\s*"([^"]+)"`)

// firmwareSurface reads the C2 surface files and returns the set of registered Berry names.
func firmwareSurface(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range c2SurfaceFiles {
		src, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			t.Fatalf("read firmware surface %s: %v (run from the package dir; the relative path "+
				"reaches the repo's firmware/main — D23.2)", f, err)
		}
		for _, m := range reRegfunc.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// setDiff returns the names present in a but absent from b. The load-bearing helper the bidirectional
// guard is built on — negatively probed in TestParityDiff_DetectsDrift below.
func setDiff(a, b map[string]bool) []string {
	var only []string
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}

// TestManifestParity is the bidirectional drift guard itself: the manifest's non-forbidden surface and
// the firmware's be_regfunc set must be equal. Green today; red the moment either side drifts.
func TestManifestParity(t *testing.T) {
	manifest := Get().NonForbiddenNames()
	firmware := firmwareSurface(t)

	if len(firmware) == 0 {
		t.Fatal("no be_regfunc sites found in the C2 surface files — the regex or the paths drifted")
	}

	if missing := setDiff(manifest, firmware); len(missing) > 0 {
		t.Errorf("manifest names with NO be_regfunc on-device (the catalog would autocomplete a "+
			"nonexistent capability): %v", missing)
	}
	if extra := setDiff(firmware, manifest); len(extra) > 0 {
		t.Errorf("be_regfunc names MISSING from the manifest (a capability the editor would never "+
			"offer — update manifest.json): %v", extra)
	}
}

// TestParityDiff_DetectsDrift is the negative probe (red→green discipline): prove setDiff catches drift
// in BOTH directions before trusting the green parity run above. A guard that cannot detect the failure
// it claims to detect is worthless.
func TestParityDiff_DetectsDrift(t *testing.T) {
	firmware := map[string]bool{"set_url": true, "reboot": true, "dev_mac": true}

	// (1) manifest declares a name the firmware does not register → flagged in manifest\firmware.
	manifestPhantom := map[string]bool{"set_url": true, "reboot": true, "dev_mac": true, "ota_trigger": true}
	if got := setDiff(manifestPhantom, firmware); len(got) != 1 || got[0] != "ota_trigger" {
		t.Errorf("setDiff must flag the phantom manifest name; got %v", got)
	}

	// (2) firmware registers a name the manifest omits → flagged in firmware\manifest.
	manifestMissing := map[string]bool{"set_url": true, "reboot": true} // dropped dev_mac
	if got := setDiff(firmware, manifestMissing); len(got) != 1 || got[0] != "dev_mac" {
		t.Errorf("setDiff must flag the unmapped firmware name; got %v", got)
	}

	// exact match → no drift either direction.
	if got := setDiff(firmware, firmware); len(got) != 0 {
		t.Errorf("equal sets must diff empty; got %v", got)
	}
}
