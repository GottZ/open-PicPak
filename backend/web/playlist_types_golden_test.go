package web

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/open-picpak/backend/internal/playliststore"
)

// A29-W7 gate — the Go↔TS drift anchor for the playlist wire vocabulary. There is no OpenAPI spec
// (design 19 §2), so the hand-maintained frontend types (src/lib/media/types.ts) are bound to their
// Go source of truth BIDIRECTIONALLY, mirroring template_types_golden_test.go. Two surface families:
//
//   - FIELD NAMES  ← the playliststore structs, SERIALISED to JSON here (json.Marshal), compared to the
//                    matching TS interface's field names. A Go json key the TS forgot → the SPA would
//                    misread a live playlist row; a phantom TS field with no Go json key → red.
//   - ENUM MEMBERS ← the playliststore validation maps (orderModes / fits / dithers), compared to the
//                    TS const arrays (ORDER_MODES / FITS / DITHERS) the editor's selects render.
//
// Pointer fields (operator_key_id, managed_serial) carry `omitempty`, so the probe instances set them
// non-nil — otherwise Marshal would drop the keys and the golden would miss a real wire field. Reuses the
// setDiff discipline (goldenDiff / assertParity) shared with the template golden in this package.
const (
	plTSTypesFile = "src/lib/media/types.ts"
	plStoreGo     = "../internal/playliststore/types.go"
)

// plTsHyphenToken captures a single-quoted token allowing hyphens (dither "floyd-steinberg").
var plTsHyphenToken = regexp.MustCompile(`'([a-zA-Z0-9_-]+)'`)

// plJSONKeys marshals v and returns its top-level JSON object keys — the literal wire field names a
// browser sees, so the golden checks exactly what ships, not the Go field identifiers.
func plJSONKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T keys: %v", v, err)
	}
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

// plTSInterfaceFields extracts the field names of `interface <name> { ... }` (a field is `name:` or
// `name?:`), anchored on `interface <name> {` so PlaylistItem/PlaylistSummary never bleed into Playlist.
func plTSInterfaceFields(t *testing.T, src, name string) map[string]bool {
	t.Helper()
	block := regexp.MustCompile(`(?s)interface ` + regexp.QuoteMeta(name) + ` \{(.*?)\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("could not locate `interface %s { ... }` in %s — the golden regex drifted", name, plTSTypesFile)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-zA-Z_][a-zA-Z0-9_]*)\??:`).FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = true
	}
	return out
}

// plGoMapKeys extracts the quoted keys of a `<varName> ... = map[string]bool{ ... }` literal.
func plGoMapKeys(t *testing.T, src, varName string) map[string]bool {
	t.Helper()
	// Anchor on `<varName> =` (whitespace only before the `=`) so a prose mention of the name in a
	// preceding comment (e.g. "// fits / dithers are …") cannot pull the match onto the wrong literal.
	block := regexp.MustCompile(`(?s)\b`+regexp.QuoteMeta(varName)+`\s*=\s*map\[string\]bool\{(.*?)\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("could not locate the %s map literal in %s — the golden regex drifted", varName, plStoreGo)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-zA-Z0-9_-]+)"`).FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = true
	}
	return out
}

// plTSArray extracts the quoted tokens of a `const <name> = [ ... ]` literal (hyphen-aware).
func plTSArray(t *testing.T, src, constName string) map[string]bool {
	t.Helper()
	block := regexp.MustCompile(`(?s)`+regexp.QuoteMeta(constName)+`[^=]*=\s*\[([^\]]*)\]`).FindStringSubmatch(src)
	if block == nil {
		t.Fatalf("could not locate the %s array literal in %s — the golden regex drifted", constName, plTSTypesFile)
	}
	out := map[string]bool{}
	for _, m := range plTsHyphenToken.FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = true
	}
	return out
}

// assertPlParity is assertParity with the correct file reference for this golden (the shared helper
// hardcodes the template file path in its message). Same bidirectional setDiff discipline (goldenDiff).
func assertPlParity(t *testing.T, surface string, goSide, tsSide map[string]bool) {
	t.Helper()
	if len(tsSide) == 0 {
		t.Fatalf("%s: the TS side is empty — %s or its golden regex drifted", surface, plTSTypesFile)
	}
	if missing := goldenDiff(goSide, tsSide); len(missing) > 0 {
		t.Errorf("%s drift: Go declares %v that %s omits (the SPA would misread a live backend value) — "+
			"add them", surface, missing, plTSTypesFile)
	}
	if extra := goldenDiff(tsSide, goSide); len(extra) > 0 {
		t.Errorf("%s drift: %s declares %v with NO Go source (a phantom wire value) — remove them or add "+
			"the Go source", surface, plTSTypesFile, extra)
	}
}

func TestPlaylistTypesGolden(t *testing.T) {
	ts := readGolden(t, plTSTypesFile)
	store := readGolden(t, plStoreGo)

	oid := int64(7)
	ms := "MANAGED1"

	// FIELD NAMES — serialise each struct and check its wire keys against the matching TS interface.
	assertPlParity(t, "Playlist fields",
		plJSONKeys(t, playliststore.Playlist{OperatorKeyID: &oid, ManagedSerial: &ms}),
		plTSInterfaceFields(t, ts, "Playlist"))
	assertPlParity(t, "PlaylistItem fields",
		plJSONKeys(t, playliststore.PlaylistItem{}),
		plTSInterfaceFields(t, ts, "PlaylistItem"))
	assertPlParity(t, "PlaylistSummary fields",
		plJSONKeys(t, playliststore.Summary{}),
		plTSInterfaceFields(t, ts, "PlaylistSummary"))

	// ENUM MEMBERS — the validation maps vs the editor's const arrays.
	assertPlParity(t, "ORDER_MODES", plGoMapKeys(t, store, "orderModes"), plTSArray(t, ts, "ORDER_MODES"))
	assertPlParity(t, "FITS", plGoMapKeys(t, store, "fits"), plTSArray(t, ts, "FITS"))
	assertPlParity(t, "DITHERS", plGoMapKeys(t, store, "dithers"), plTSArray(t, ts, "DITHERS"))
}

// TestPlaylistTypesParsers_Extract is the negative probe: prove the field/enum extractors actually see
// the tokens (an empty extraction would make assertParity vacuously pass in the TS-empty guard only, so
// pin non-empty extraction + a known member for each surface) before trusting the green parity run above.
func TestPlaylistTypesParsers_Extract(t *testing.T) {
	ts := readGolden(t, plTSTypesFile)
	store := readGolden(t, plStoreGo)

	if f := plTSInterfaceFields(t, ts, "Playlist"); !f["order_mode"] || !f["operator_key_id"] {
		t.Errorf("Playlist interface parser missed a field: got %v", f)
	}
	if f := plTSInterfaceFields(t, ts, "PlaylistItem"); !f["image_id"] || !f["position"] {
		t.Errorf("PlaylistItem interface parser missed a field: got %v", f)
	}
	if d := plGoMapKeys(t, store, "dithers"); !d["floyd-steinberg"] {
		t.Errorf("dithers map parser missed the hyphenated member: got %v", d)
	}
	if a := plTSArray(t, ts, "DITHERS"); !a["floyd-steinberg"] {
		t.Errorf("DITHERS array parser missed the hyphenated member: got %v", a)
	}
}
