package main

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/faasstore"
)

// faas_ui_http.go is the FaaS editor's server support (Doc 25) — the endpoints A24 deliberately left to
// the editor wave (24:39-42). A24 ships the function-store CRUD + the binding WRITE (registerFaasRoutes);
// Doc 25 adds the binding READS (forward + reverse/blast-radius) and the unbind here. The test-run
// execution route (POST /api/functions/test-run) lands in W3 — this W1 file is read/unbind only, no
// execution path.

type faasUIHandlers struct {
	pool *pgxpool.Pool
}

// registerFaasUIRoutes mounts the Doc 25 §4.4 binding endpoints. Reads are auth-gated (any valid key may
// inspect which function a device renders / a function's blast radius); the unbind is a mutation →
// requireAdmin — the exact gating shape registerFaasRoutes / registerOTARoutes apply. Single source of
// the wiring so main.go and the gating test can never drift. The GET/DELETE here share the
// `/api/devices/{serial}/render` path with A24's PUT (distinct method patterns, no conflict).
func registerFaasUIRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := faasUIHandlers{pool: pool}
	mux.Handle("GET /api/devices/{serial}/render", adminhttp.Auth(pool)(http.HandlerFunc(h.boundFunction)))
	mux.Handle("GET /api/functions/{id}/devices", adminhttp.Auth(pool)(http.HandlerFunc(h.boundDevices)))
	mux.Handle("DELETE /api/devices/{serial}/render", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.unbind))))
}

// boundFunction — GET /api/devices/{serial}/render (auth): the function a device renders (forward read,
// §4.4). Returns {binding:{function_id,name}} or {binding:null} — never the source. The serial has no FK
// to devices (0009), so a binding may name a not-yet-onboarded device; null means simply "no binding".
func (h faasUIHandlers) boundFunction(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	id, name, ok, err := faasstore.BoundFunction(r.Context(), h.pool, serial)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "binding lookup failed")
		return
	}
	if !ok {
		adminhttp.WriteOK(w, r, map[string]any{"binding": nil})
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"binding": map[string]any{"function_id": id, "name": name}})
}

// boundDevices — GET /api/functions/{id}/devices (auth): the devices bound to a function — the blast
// radius (D25.9): every device a source edit re-renders on its next poll. Returns {serials:[...],count}.
// An unknown function is a 404, not a silent {count:0} — the editor's badge must never under-warn a
// fleet-wide edit because of a typo'd id (T12).
func (h faasUIHandlers) boundDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := faasID(w, r)
	if !ok {
		return
	}
	if _, err := faasstore.Version(r.Context(), h.pool, id); errors.Is(err, faasstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function check failed")
		return
	}
	serials, err := faasstore.BoundSerials(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "bound-device lookup failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serials": serials, "count": len(serials)})
}

// unbind — DELETE /api/devices/{serial}/render (admin): drop a device's render binding (A24 ships only the
// PUT). The device falls back to "no function"; the last-good frame is left intact (a re-bind reuses it —
// see faasstore.Unbind). 404 if the device had no binding.
func (h faasUIHandlers) unbind(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	found, err := faasstore.Unbind(r.Context(), h.pool, serial)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "unbind failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no binding for that device")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": serial, "unbound": true})
}
