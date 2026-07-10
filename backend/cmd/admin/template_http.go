package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/templatestore"
)

type templateHandlers struct {
	pool *pgxpool.Pool
}

// registerTemplateRoutes mounts the 5 template-store CRUD routes (§4.2) plus the /apply dispatch. Reads are
// auth-gated (any valid key — a read-only operator may browse the catalog and load a template into the
// editor); mutations require admin, the exact gating main.go applies to the function/OTA routes. /apply is
// RCE-equivalent (it enqueues a device script / mints a runnable function), so it carries the same
// RequireAdmin gate as POST /api/command. Single source of the wiring so main.go and the gating test can
// never drift (mirror of registerFaasRoutes). The source is foreign code at rest, never eval'd here (only
// the worker / the C2 VM runs it, D24.7).
func registerTemplateRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := templateHandlers{pool: pool}
	mux.Handle("GET /api/templates", adminhttp.Auth(pool)(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/templates/{id}", adminhttp.Auth(pool)(http.HandlerFunc(h.get)))
	mux.Handle("POST /api/templates", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.create))))
	mux.Handle("PUT /api/templates/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.update))))
	mux.Handle("DELETE /api/templates/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.delete))))
	mux.Handle("POST /api/templates/{id}/apply", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.apply))))
}

// list — GET /api/templates (auth): the list-view projection (no source, no params, no trust profile —
// the picker view, §6). Optional ?kind= filters via the composite (kind,id) index; ?after=/?limit= walk
// the keyset. An unknown kind simply matches no rows (empty list), not an error.
func (h templateHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.ParseInt(q.Get("limit"), 10, 64)
	sums, err := templatestore.List(r.Context(), h.pool, kind, limit, after)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"templates": sums})
}

// get — GET /api/templates/{id} (auth): the full template (source + params + trust profile). builtin=true
// is on the payload so the SPA can badge a prefab row (its editor treats "load" as "duplicate to an
// operator template", since a mutation of the row itself is refused, §5.3).
func (h templateHandlers) get(w http.ResponseWriter, r *http.Request) {
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
	adminhttp.WriteOK(w, r, map[string]any{"template": t})
}

// create — POST /api/templates (admin): create an operator-authored template (builtin=false, version=1).
// Validation lands as 422 before the DB round-trip (bad name — incl. the reserved 'builtin/' namespace —
// bad kind, or a berry_snippet source over the 8191-byte cap); a duplicate name surfaces as 409. The
// trust-profile fields (egress_allow/secret_bindings/trigger_config) are stored as data — they grant
// nothing until an /apply wave (W5) reaches faasstore.Create with them.
func (h templateHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string          `json:"name"`
		Kind           string          `json:"kind"`
		Source         string          `json:"source"`
		Params         json.RawMessage `json:"params"`
		EgressAllow    []string        `json:"egress_allow"`
		SecretBindings []string        `json:"secret_bindings"`
		TriggerConfig  json.RawMessage `json:"trigger_config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Source == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_source", "source is required")
		return
	}
	id, err := templatestore.Create(r.Context(), h.pool, templatestore.CreateParams{
		Name: body.Name, Kind: body.Kind, Source: body.Source, Params: body.Params,
		EgressAllow: body.EgressAllow, SecretBindings: body.SecretBindings, TriggerConfig: body.TriggerConfig,
	})
	if err != nil {
		if code, msg, ok := templateCreateErr(err); ok {
			adminhttp.WriteErr(w, r, code, msg.code, msg.text)
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template create failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "name": body.Name, "kind": body.Kind, "builtin": false})
}

// update — PUT /api/templates/{id} (admin): replace the mutable body (source/params/trust profile) and
// BUMP version (editor history). A builtin row is IMMUTABLE (the embedded catalog is its source of truth,
// §5.3) → 409 builtin_immutable; an unknown id → 404; a berry_snippet grown past 8191 bytes → 422. The
// row is loaded first so the builtin/404 decision precedes the write (and never leaks a partial update).
func (h templateHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	t, done := h.loadMutable(w, r, id)
	if done {
		return
	}
	var body struct {
		Source         string          `json:"source"`
		Params         json.RawMessage `json:"params"`
		EgressAllow    []string        `json:"egress_allow"`
		SecretBindings []string        `json:"secret_bindings"`
		TriggerConfig  json.RawMessage `json:"trigger_config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Source == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_source", "source is required")
		return
	}
	found, err := templatestore.Update(r.Context(), h.pool, id, templatestore.UpdateParams{
		Source: body.Source, Params: body.Params, EgressAllow: body.EgressAllow,
		SecretBindings: body.SecretBindings, TriggerConfig: body.TriggerConfig,
	})
	if errors.Is(err, templatestore.ErrScriptTooLong) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "berry_too_long",
			"berry_snippet source exceeds the 8191-byte cap")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template update failed")
		return
	}
	if !found { // raced with a concurrent delete after the load
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such template")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "updated": true, "version": t.Version + 1})
}

// delete — DELETE /api/templates/{id} (admin): drop an operator template. A builtin row is immutable →
// 409; an unknown id → 404. The faas_functions.template_id FK is ON DELETE SET NULL, so a function
// already applied from this template is never severed (templates are blueprints, not live bindings).
func (h templateHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := templateID(w, r)
	if !ok {
		return
	}
	if _, done := h.loadMutable(w, r, id); done {
		return
	}
	found, err := templatestore.Delete(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template delete failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such template")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "deleted": true})
}

// --- helpers ---

// loadMutable loads the row a mutation targets and gates it: it writes a 404 (unknown id), a 500 (load
// error) or a 409 builtin_immutable (prefab row) and returns done=true when the caller must stop. On a
// mutable operator row it returns the template and done=false. This is the single builtin-immutability
// gate shared by update + delete (§5.3), so the two paths can never drift on the refusal.
func (h templateHandlers) loadMutable(w http.ResponseWriter, r *http.Request, id int64) (*templatestore.Template, bool) {
	t, err := templatestore.Load(r.Context(), h.pool, id)
	if errors.Is(err, templatestore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such template")
		return nil, true
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "template load failed")
		return nil, true
	}
	if t.Builtin {
		adminhttp.WriteErr(w, r, http.StatusConflict, "builtin_immutable",
			"builtin templates are immutable — duplicate to an operator template to edit")
		return nil, true
	}
	return t, false
}

type errCode struct{ code, text string }

// templateCreateErr maps a store Create error to a (status, {code,text}, ok) triple. Unknown errors
// return ok=false so the caller falls through to a 500. Keeps the store the single validation authority
// (name charset + 'builtin/' reservation, kind enum, berry byte-cap) while the handler owns the wire code.
func templateCreateErr(err error) (int, errCode, bool) {
	switch {
	case errors.Is(err, templatestore.ErrInvalidName):
		return http.StatusUnprocessableEntity, errCode{"invalid_name",
			`name must match ^[a-z0-9][a-z0-9._-]{0,127}$ and not use the reserved 'builtin/' namespace`}, true
	case errors.Is(err, templatestore.ErrInvalidKind):
		return http.StatusUnprocessableEntity, errCode{"invalid_kind",
			"kind must be render_fn|berry_snippet|playlist_preset"}, true
	case errors.Is(err, templatestore.ErrScriptTooLong):
		return http.StatusUnprocessableEntity, errCode{"berry_too_long",
			"berry_snippet source exceeds the 8191-byte cap"}, true
	case templatestore.IsUniqueViolation(err):
		return http.StatusConflict, errCode{"duplicate_name", "a template with that name already exists"}, true
	}
	return 0, errCode{}, false
}

// templateID parses {id} and writes a 422 (+ returns false) on a non-integer / non-positive path value
// (mirror of faasID).
func templateID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_id", "template id must be a positive integer")
		return 0, false
	}
	return id, true
}
