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
	"github.com/open-picpak/backend/internal/faasstore"
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
	case templatestore.KindRenderFn:
		h.applyRenderFn(w, r, t, body)
	default:
		// playlist_preset is the Playlist axis's materialisation (A29, §9) — a clean 422 here, not a panic.
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
	script, serr := templatestore.Substitute(t, params)
	if serr != nil {
		writeSubstituteErr(w, r, serr)
		return
	}
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

// applyRenderFn mints a faas_functions row from a render_fn template (§4.3). The substituted source, the
// template's trust profile (egress_allow / secret_bindings / trigger_config) and a template_id provenance
// stamp all flow into faasstore.Create — without the trust-profile passthrough the minted function would
// fetch with an empty allow-list and the egress proxy would hard-block it (a dead Deliverable-4 function).
// The function name is repeatable at fleet scale: an explicit fn_name collides as 409 function_name_taken,
// a derived name ({template}-{param-hash}) retries with a numeric suffix so a second apply never dies on the
// name UNIQUE. Fleet semantics are n:1 — ONE function, then a bind per target serial (no "*" fanout, the
// render binding has no fleet row, §4.3).
func (h templateHandlers) applyRenderFn(w http.ResponseWriter, r *http.Request, t *templatestore.Template, body applyBody) {
	params, perr := decodeParams(body.Params)
	if perr != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "params must be a JSON object")
		return
	}
	source, serr := templatestore.Substitute(t, params)
	if serr != nil {
		writeSubstituteErr(w, r, serr)
		return
	}

	explicit := body.FnName != ""
	base := body.FnName
	if !explicit {
		base = deriveFnName(t.Name, params)
	}
	if !faasstore.ValidName(base) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_name",
			`fn_name must match ^[a-z0-9][a-z0-9._-]{0,127}$ (the reserved 'builtin/' namespace is rejected by the charset)`)
		return
	}

	// Bind targets are explicit serials (never "*": the render binding has no fleet row). Validate up front
	// so a bad serial fails before a function is minted.
	if body.Bind {
		for _, serial := range body.TargetSerials {
			if !devicestore.ValidSerial(serial) {
				adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
					"serial must be a valid device serial (render_fn bind has no '*' fanout): "+serial)
				return
			}
		}
	}

	newID, name, done := h.createRenderFn(w, r, t, source, base, explicit)
	if done {
		return
	}

	serials := []string{}
	if body.Bind {
		for _, serial := range body.TargetSerials {
			if err := faasstore.BindDevice(r.Context(), h.pool, serial, newID); err != nil {
				adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "bind failed for "+serial)
				return
			}
			serials = append(serials, serial)
		}
	}
	adminhttp.WriteOK(w, r, map[string]any{"kind": t.Kind, "id": newID, "name": name, "serials": serials})
}

// createRenderFn inserts the function and resolves a name collision. An explicit fn_name that collides is a
// terminal 409 function_name_taken (the operator chose it). A derived name retries with a -2/-3… suffix so a
// repeat apply of the same template mints a fresh function instead of dying on the name UNIQUE. It writes
// the error response and returns done=true on failure.
func (h templateHandlers) createRenderFn(w http.ResponseWriter, r *http.Request, t *templatestore.Template, source, base string, explicit bool) (int64, string, bool) {
	for attempt := 0; attempt < 64; attempt++ {
		name := base
		if attempt > 0 {
			name = fmt.Sprintf("%s-%d", base, attempt+1) // -2, -3, …
		}
		id, err := faasstore.Create(r.Context(), h.pool, faasstore.CreateParams{
			Name: name, Source: source,
			TriggerConfig: t.TriggerConfig, SecretBindings: t.SecretBindings, EgressAllow: t.EgressAllow,
			TemplateID: &t.ID,
		})
		if err == nil {
			return id, name, false
		}
		if faasstore.IsUniqueViolation(err) {
			if explicit {
				adminhttp.WriteErr(w, r, http.StatusConflict, "function_name_taken",
					"a function with that name already exists")
				return 0, "", true
			}
			continue // derived name: try the next suffix
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function create failed")
		return 0, "", true
	}
	adminhttp.WriteErr(w, r, http.StatusConflict, "function_name_taken",
		"could not derive a free function name after 64 attempts")
	return 0, "", true
}

// deriveFnName builds {template-name without the 'builtin/' prefix}-{param-hash} (§4.3). The hash keeps the
// name stable for a given param set and short enough to stay inside the 128-char charset.
func deriveFnName(templateName string, params map[string]any) string {
	base := strings.TrimPrefix(templateName, "builtin/")
	canon, _ := json.Marshal(sortedParams(params))
	sum := sha256.Sum256(canon)
	hash := fmt.Sprintf("%x", sum[:4]) // 8 hex chars
	if max := 128 - 1 - len(hash); len(base) > max {
		base = base[:max]
	}
	return base + "-" + hash
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

// writeSubstituteErr maps a templatestore.Substitute failure to a response. A *SubstituteError is a
// defined client fault (unknown_param / missing_param / unresolved_placeholder / egress_host_mismatch /
// invalid_param) → 422 with the store's wire code; anything else is a malformed stored schema → 500
// config defect.
func writeSubstituteErr(w http.ResponseWriter, r *http.Request, err error) {
	var se *templatestore.SubstituteError
	if errors.As(err, &se) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, se.Code, se.Msg)
		return
	}
	adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template substitution failed")
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
