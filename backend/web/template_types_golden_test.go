package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/open-picpak/backend/internal/templatestore"
)

// A30-W7 gate — the Go↔TS drift anchor for the template picker. There is no OpenAPI spec (design 19 §2),
// so the hand-maintained frontend wire vocabulary (src/lib/templates/types.ts) is bound to its Go source
// of truth BIDIRECTIONALLY: a Go value the TS forgot to mirror → red (the picker would misread a live
// backend value); a phantom TS value with no Go source → red. Three surfaces drift-guarded:
//
//   - KINDS             ← templatestore.Kind* constants (compiled in, imported here)
//   - PARAM_TYPES       ← templatestore.checkParamType switch cases (substitute.go) + the "string" pass-through
//   - APPLY_ERROR_CODES ← templatestore.SubstituteError.Code literals (substitute.go) + apply's "berry_too_long"
//
// Same setDiff discipline as internal/berry/parity_test.go. Red-proof: delete a kind from the KINDS
// array in types.ts and re-run → this test fails in the Go\TS direction (documented in the wave report).
const (
	tsTypesFile  = "src/lib/templates/types.ts"
	substituteGo = "../internal/templatestore/substitute.go"
)

var (
	// reParamCase captures the string switch cases in checkParamType (case "url": / "number": / "enum":).
	reParamCase = regexp.MustCompile(`case "([a-z]+)":`)
	// reSubstErr captures every SubstituteError.Code literal (the /apply param-path 422 wire codes).
	reSubstErr = regexp.MustCompile(`SubstituteError\{Code:\s*"([a-z_]+)"`)
	// reTsToken captures a single-quoted lower_snake token inside a TS array literal.
	reTsToken = regexp.MustCompile(`'([a-zA-Z_]+)'`)
)

func readGolden(t *testing.T, p string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatalf("read %s: %v (run from the package dir; the relative paths reach the repo tree)", p, err)
	}
	return string(src)
}

// tsArrayValues extracts the quoted tokens of the array literal assigned to `const <name>`. It anchors on
// the `=` so the `[]` in a `readonly Foo[]` type annotation is not mistaken for the array (that empty
// literal sits before the `=`).
func tsArrayValues(t *testing.T, src, constName string) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(constName) + `[^=]*=\s*\[([^\]]*)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not locate the %s array literal in types.ts — the golden regex drifted", constName)
	}
	out := map[string]bool{}
	for _, tok := range reTsToken.FindAllStringSubmatch(m[1], -1) {
		out[tok[1]] = true
	}
	return out
}

// captures returns the set of capture-group-1 matches of re over src.
func captures(re *regexp.Regexp, src string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

func set(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// goldenDiff returns the names present in a but absent from b (sorted). The load-bearing helper — negatively
// probed in TestTemplateTypesDiff_DetectsDrift.
func goldenDiff(a, b map[string]bool) []string {
	var only []string
	for k := range a {
		if !b[k] {
			only = append(only, k)
		}
	}
	sort.Strings(only)
	return only
}

func assertParity(t *testing.T, surface string, goSide, tsSide map[string]bool) {
	t.Helper()
	if len(tsSide) == 0 {
		t.Fatalf("%s: the TS side is empty — types.ts or its golden regex drifted", surface)
	}
	if missing := goldenDiff(goSide, tsSide); len(missing) > 0 {
		t.Errorf("%s drift: Go declares %v that src/lib/templates/types.ts omits (the SPA would misread a "+
			"live backend value) — add them to types.ts", surface, missing)
	}
	if extra := goldenDiff(tsSide, goSide); len(extra) > 0 {
		t.Errorf("%s drift: types.ts declares %v with NO Go source (a phantom wire value) — remove them or "+
			"add the Go source", surface, extra)
	}
}

func TestTemplateTypesGolden(t *testing.T) {
	ts := readGolden(t, tsTypesFile)
	sub := readGolden(t, substituteGo)

	// KINDS ← the templatestore kind constants (compiled in).
	goKinds := set(templatestore.KindRenderFn, templatestore.KindBerrySnippet, templatestore.KindPlaylistPreset)
	assertParity(t, "KINDS", goKinds, tsArrayValues(t, ts, "KINDS"))

	// PARAM_TYPES ← checkParamType's switch cases (url/number/enum) + the "string" pass-through (the default
	// case, which is not a literal in the switch, so it is the one pinned member).
	goParamTypes := captures(reParamCase, sub)
	goParamTypes["string"] = true
	assertParity(t, "PARAM_TYPES", goParamTypes, tsArrayValues(t, ts, "PARAM_TYPES"))

	// APPLY_ERROR_CODES ← SubstituteError.Code literals + apply's berry_too_long (the post-substitution octet
	// cap, raised in template_apply_http.go, not substitute.go — so it is the one pinned member).
	goCodes := captures(reSubstErr, sub)
	goCodes["berry_too_long"] = true
	assertParity(t, "APPLY_ERROR_CODES", goCodes, tsArrayValues(t, ts, "APPLY_ERROR_CODES"))
}

// TestTemplateTypesDiff_DetectsDrift is the negative probe: prove goldenDiff catches drift in BOTH
// directions before trusting the green parity run above (mirror of berry/parity_test's TestParityDiff).
func TestTemplateTypesDiff_DetectsDrift(t *testing.T) {
	base := set("a", "b")

	if got := goldenDiff(base, set("a")); len(got) != 1 || got[0] != "b" {
		t.Errorf("goldenDiff must flag the value missing from the second set; got %v", got)
	}
	if got := goldenDiff(set("a", "z"), base); len(got) != 1 || got[0] != "z" {
		t.Errorf("goldenDiff must flag the phantom value; got %v", got)
	}
	if got := goldenDiff(base, base); len(got) != 0 {
		t.Errorf("equal sets must diff empty; got %v", got)
	}
}
