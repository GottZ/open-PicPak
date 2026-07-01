package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

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
	pool       *pgxpool.Pool
	testClient *http.Client // admin→supervisor test-render seam; nil ⇒ FAAS_TEST_SOCK unset ⇒ route 503s
}

// registerFaasUIRoutes mounts the Doc 25 §4.4 binding endpoints. Reads are auth-gated (any valid key may
// inspect which function a device renders / a function's blast radius); the unbind is a mutation →
// requireAdmin — the exact gating shape registerFaasRoutes / registerOTARoutes apply. Single source of
// the wiring so main.go and the gating test can never drift. The GET/DELETE here share the
// `/api/devices/{serial}/render` path with A24's PUT (distinct method patterns, no conflict).
func registerFaasUIRoutes(mux *http.ServeMux, pool *pgxpool.Pool, testSock string) {
	h := faasUIHandlers{pool: pool, testClient: newTestRenderClient(testSock)}
	mux.Handle("GET /api/devices/{serial}/render", adminhttp.Auth(pool)(http.HandlerFunc(h.boundFunction)))
	mux.Handle("GET /api/functions/{id}/devices", adminhttp.Auth(pool)(http.HandlerFunc(h.boundDevices)))
	mux.Handle("DELETE /api/devices/{serial}/render", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.unbind))))
	// test-run is RCE-equivalent (it evaluates foreign code) → requireAdmin + attributed (D25.3).
	mux.Handle("POST /api/functions/test-run", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.testRun))))
}

// testRunTimeout bounds one test render (worker cold-start + M4 overhead ~18 s; a generous outer bound).
const testRunTimeout = 40 * time.Second

// newTestRenderClient builds the HTTP-over-UDS client for the admin→supervisor test-render seam, or nil
// when FAAS_TEST_SOCK is unset (the route then 503s — pausability-safe: no test-run until compose wires it).
func newTestRenderClient(sock string) *http.Client {
	if sock == "" {
		return nil
	}
	return &http.Client{
		Timeout: testRunTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// testRun — POST /api/functions/test-run (admin): the FaaS analog of the RCE-capable enqueue (D25.3). The
// route is requireAdmin-gated (wiring above); this handler ATTRIBUTES the run to the operator key (a loud
// audit log), resolves ctx.channel SERVER-SIDE (D20.5), and forwards to the supervisor test-render arm over
// FAAS_TEST_SOCK. The supervisor runs it in the SAME worker sandbox as prod, secrets STUBBED (D25.12),
// side-effect-free (D25.4). The framed binary response (meta+packed+raw) is forwarded verbatim; the browser
// decodes it. It grants no capability an admin lacks — it only shortens the author→see-frame loop (D25.3).
func (h faasUIHandlers) testRun(w http.ResponseWriter, r *http.Request) {
	if h.testClient == nil {
		adminhttp.WriteErr(w, r, http.StatusServiceUnavailable, "test_run_unavailable",
			"test-run is not configured (FAAS_TEST_SOCK unset)")
		return
	}
	var body struct {
		Fn struct {
			ID             *int64          `json:"id"`
			Source         string          `json:"source"`
			Dither         string          `json:"dither"`
			SecretBindings []string        `json:"secret_bindings"`
			EgressAllow    []string        `json:"egress_allow"`
			Limits         json.RawMessage `json:"limits"`
		} `json:"fn"`
		Ctx struct {
			Serial  string          `json:"serial"`
			Trigger string          `json:"trigger"`
			Now     string          `json:"now"`
			Payload json.RawMessage `json:"payload"`
		} `json:"ctx"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if !devicestore.ValidSerial(body.Ctx.Serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`ctx.serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	trigger := body.Ctx.Trigger
	if trigger == "" {
		trigger = "render"
	}
	if trigger != "render" && trigger != "schedule" && trigger != "webhook" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_trigger",
			"ctx.trigger must be render|schedule|webhook")
		return
	}
	if len(body.Ctx.Payload) > 0 && !json.Valid(body.Ctx.Payload) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_payload", "ctx.payload must be valid JSON")
		return
	}
	if body.Fn.ID == nil && body.Fn.Source == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_source", "fn.id or fn.source is required")
		return
	}
	// ctx.channel is resolved SERVER-SIDE from devices.channel (D20.5), never a client value.
	var channel string
	_ = h.pool.QueryRow(r.Context(), `SELECT channel FROM devices WHERE serial = $1`, body.Ctx.Serial).Scan(&channel)
	// attribution (D25.3) — the enqueue-attribution discipline (D17.9), applied to the execute route.
	op, _ := adminhttp.Operator(r.Context())
	fnLabel := "adhoc"
	if body.Fn.ID != nil {
		fnLabel = strconv.FormatInt(*body.Fn.ID, 10)
	}
	log.Printf("faas test-run key=%d fn=%s serial=%s", op.KeyID, fnLabel, body.Ctx.Serial)

	seam, _ := json.Marshal(map[string]any{
		"id": body.Fn.ID, "source": body.Fn.Source, "dither": body.Fn.Dither,
		"secret_bindings": body.Fn.SecretBindings, "egress_allow": body.Fn.EgressAllow, "limits": body.Fn.Limits,
		"serial": body.Ctx.Serial, "channel": channel, "trigger": trigger, "now": body.Ctx.Now, "payload": body.Ctx.Payload,
	})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://faas/test-render", bytes.NewReader(seam))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "seam request build failed")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.testClient.Do(req)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadGateway, "supervisor_unreachable", "test-render supervisor unreachable")
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	}
	if resp.StatusCode != http.StatusOK {
		adminhttp.WriteErr(w, r, http.StatusBadGateway, "supervisor_error", "test-render failed")
		return
	}
	// forward the framed binary response verbatim (u32 metaLen | meta | packed | raw); the browser decodes it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
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
