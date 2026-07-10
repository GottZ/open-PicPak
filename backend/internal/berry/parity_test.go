package berry

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// regfuncNames reads one firmware surface file and returns the set of be_regfunc-registered Berry names.
func regfuncNames(t *testing.T, files ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range files {
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

// firmwareSurface reads the C2 surface files and returns the set of registered Berry names.
func firmwareSurface(t *testing.T) map[string]bool {
	t.Helper()
	return regfuncNames(t, c2SurfaceFiles...)
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

// fbSurfaceFile is the render-phase drawing surface (fb.c). Its be_regfunc names are the "forbidden"
// render class in the manifest — NOT in the C2 VM (cmd.c registers no fb surface), so a berry_snippet
// that calls one faults on-device while the cursor still advances. A30-W7 closes the drift the parity
// surface above deliberately excludes: the manifest declared only 7 of fb.c's 10 render names
// (text16/qr/dump were missing), so the editor lint marked them "unknown" instead of "forbidden with a
// phase hint". This binds the manifest's render-phase forbidden set to fb.c bidirectionally.
const fbSurfaceFile = "../../../firmware/main/fb.c"

// renderPhaseForbiddenNames is the manifest's forbidden render-phase surface: class == forbidden AND a
// Render-phase doc (the policy-phase net names — wifi_*/http_get from netberry.c — carry a Policy-phase
// doc and are out of this test's scope). Mirrors catalog.ts phaseOf's Render-phase discriminator.
func renderPhaseForbiddenNames() map[string]bool {
	out := map[string]bool{}
	for _, c := range Get().Capabilities {
		if c.Class == ClassForbidden && strings.HasPrefix(c.Doc, "Render-phase") {
			out[c.Name] = true
		}
	}
	return out
}

// TestForbiddenFbParity binds the manifest's render-phase forbidden names to fb.c's be_regfunc sites in
// BOTH directions — the A30-W7 gate. A render name registered in fb.c but absent from the manifest (the
// text16/qr/dump gap this wave closes) → red; a manifest render-phase name with no fb.c registration →
// red. Red-proof: delete the "text16" line from manifest.json and re-run → this test fails in the
// firmware\manifest direction.
func TestForbiddenFbParity(t *testing.T) {
	manifest := renderPhaseForbiddenNames()
	fb := regfuncNames(t, fbSurfaceFile)

	if len(fb) == 0 {
		t.Fatal("no be_regfunc sites found in fb.c — the regex or the path drifted")
	}
	if missing := setDiff(manifest, fb); len(missing) > 0 {
		t.Errorf("manifest render-phase names with NO be_regfunc in fb.c (a phantom forbidden entry): %v", missing)
	}
	if extra := setDiff(fb, manifest); len(extra) > 0 {
		t.Errorf("fb.c render names MISSING from the manifest forbidden class (the editor would mark them "+
			"'unknown' instead of 'forbidden with a phase hint' — add them to manifest.json): %v", extra)
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
