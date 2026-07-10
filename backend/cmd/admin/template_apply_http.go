package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/commandstore"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/templatestore"
)

// applyBody is the POST /api/templates/{id}/apply request (§4.3). The dispatch is server-authoritative on
// the row's kind (never a body field, §5.2) — a request names only the id + how to apply it. params drive
// the {{name}} substitution (§4.4); target_serials is the device set ("*" = the whole fleet for a
// berry_snippet, an explicit n:1 set for a render_fn); fn_name + bind are render_fn-only (W5).
type applyBody struct {
	FnName         string          `json:"fn_name"`         // render_fn: explicit function name (W5)
	Params         json.RawMessage `json:"params"`          // {{name}} substitution values
	TargetSerials  []string        `json:"target_serials"`  // devices (or ["*"] fleet) to apply to
	Bind           bool            `json:"bind"`            // render_fn: bind target_serials to the new function (W5)
	IdempotencyKey *string         `json:"idempotency_key"` // optional override of the derived dedup key
}

// apply — POST /api/templates/{id}/apply (admin): instantiate a template onto devices. RCE-equivalent
// (it enqueues a device script / mints a runnable function), so it carries the same RequireAdmin gate as
// POST /api/command. The row's kind selects the branch — a berry_snippet enqueues onto the C2 queue (W4),
// a render_fn mints a faas_functions row (W5), a playlist_preset is the Playlist axis's job (A29, §9), so
// it is a defined 422 here, never a panic path (§5.2).
func (h templateHandlers) apply(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	t, err := templatestore.Load(r.Context(), h.pool, id)
	if errors.Is(err, templatestore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such template")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template load failed")
		return
	}
	var body applyBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}

	switch t.Kind {
	case templatestore.KindBerrySnippet:
		h.applyBerry(w, r, t, body)
	default:
		// render_fn arrives in W5; playlist_preset belongs to A29 (§9). Both are a clean 422, not a panic.
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unsupported_kind",
			"apply is not available for kind "+t.Kind)
	}
}

// applyBerry enqueues a berry_snippet onto the C2 queue for each target serial ("*" = fleet). The source is
// substituted (§4.4) and the byte cap is RE-checked AFTER substitution — a stored source under 8191 bytes
// can cross the cap once a multibyte param is spliced in (§5.1 guard 3), so the pre-substitution CHECK on
// the row is not enough; that post-substitution octet check is the poison-pill gate. Each enqueue carries a
// derived idempotency key (tmpl:{id}:{param-hash}), so a double-clicked identical apply dedups to one
// pending row per serial (K4) while a changed param set produces a new key → a new row.
func (h templateHandlers) applyBerry(w http.ResponseWriter, r *http.Request, t *templatestore.Template, body applyBody) {
	if len(body.TargetSerials) == 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_target",
			`target_serials is required (use ["*"] to apply to the whole fleet)`)
		return
	}
	params, perr := decodeParams(body.Params)
	if perr != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "params must be a JSON object")
		return
	}
	script := substitute(t.Source, params)
	// octet_length, not codepoints: len() on a Go string is bytes, mirroring the firmware fetch cap. A
	// multibyte param that pushes the substituted script past 8191 B is the poison pill this rejects (W18).
	if len(script) > templatestore.BerryScriptMax {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "berry_too_long",
			"berry_snippet source exceeds the 8191-byte cap after substitution")
		return
	}
	key := body.IdempotencyKey
	if key == nil {
		derived := deriveIdempotencyKey(t.ID, params)
		key = &derived
	}
	note := fmt.Sprintf("tmpl:%s", t.Name)
	op, _ := adminhttp.Operator(r.Context())

	results := make([]map[string]any, 0, len(body.TargetSerials))
	for _, serial := range body.TargetSerials {
		if serial != "*" && !devicestore.ValidSerial(serial) {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
				"serial must be a valid device serial or '*': "+serial)
			return
		}
		seq, err := commandstore.Enqueue(r.Context(), h.pool, serial, script, &note, op.KeyID, key)
		if err != nil {
			switch {
			case errors.Is(err, commandstore.ErrScriptEmpty), errors.Is(err, commandstore.ErrScriptTooLong):
				adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_script", err.Error())
			case errors.Is(err, commandstore.ErrUnknownSerial):
				adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_serial", err.Error())
			default:
				adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "enqueue failed")
			}
			return
		}
		results = append(results, map[string]any{"serial": serial, "seq": seq})
	}
	adminhttp.WriteOK(w, r, map[string]any{"kind": t.Kind, "enqueued": results})
}

// --- substitution + idempotency helpers (§4.4 basic mechanic; full schema validation lands in W6) ---

// decodeParams unmarshals the apply body's params object. An absent/empty body yields an empty map (an
// unparametrised template applies verbatim). A non-object payload is a caller error.
func decodeParams(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// substitute replaces every {{name}} token in source with its param value (pure string replacement, §4.4).
// The param value goes in as data to code that is already treated as foreign (the C2 safe subset / the
// worker sandbox) — the sandbox, not the substitution, is the isolation boundary. Full schema validation
// (required / unknown / unresolved-token / url-host) is W6; this is the byte-exact splice W4/W5 need.
func substitute(source string, params map[string]any) string {
	if len(params) == 0 {
		return source
	}
	pairs := make([]string, 0, len(params)*2)
	for name, val := range params {
		pairs = append(pairs, "{{"+name+"}}", fmt.Sprint(val))
	}
	return strings.NewReplacer(pairs...).Replace(source)
}

// deriveIdempotencyKey builds tmpl:{id}:{param-hash} (§4.3/E-A30-5). json.Marshal of a map sorts its keys,
// so the hash is stable for an identical param set (→ dedup to one pending row) and differs the moment a
// value changes (→ a new row is allowed through). The key is per-serial via the command_queue partial
// UNIQUE (serial, idempotency_key), so a fleet "*" apply dedups fleet-wide while each explicit serial keeps
// its own pending slot.
func deriveIdempotencyKey(id int64, params map[string]any) string {
	canon, _ := json.Marshal(sortedParams(params))
	sum := sha256.Sum256(canon)
	return fmt.Sprintf("tmpl:%d:%x", id, sum[:8])
}

// sortedParams returns params as an ordered slice of key/value pairs so the hash input is deterministic
// even if a future marshaller stops sorting map keys.
func sortedParams(params map[string]any) [][2]any {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]any{k, params[k]})
	}
	return out
}
