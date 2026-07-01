package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/faasstore"
)

type faasHandlers struct {
	pool *pgxpool.Pool
}

// registerFaasRoutes mounts the 7 FaaS function-store routes (§4.8). Reads are auth-gated (any valid key
// — a read-only operator may inspect a function's source/config/bindings); mutations require admin, the
// exact gating main.go applies to the device/OTA routes. Single source of the wiring so main.go and the
// gating test can never drift (mirror of registerOTARoutes). Doc 25 (the editor/test-run UI) is a
// separate wave — this is the store write path only; the source is foreign code at rest, never eval'd
// here (only the worker evaluates it, D24.7).
func registerFaasRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := faasHandlers{pool: pool}
	mux.Handle("GET /api/functions", adminhttp.Auth(pool)(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/functions/{id}", adminhttp.Auth(pool)(http.HandlerFunc(h.get)))
	mux.Handle("POST /api/functions", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.create))))
	mux.Handle("PUT /api/functions/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.update))))
	mux.Handle("PATCH /api/functions/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.patch))))
	mux.Handle("DELETE /api/functions/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.delete))))
	mux.Handle("PUT /api/devices/{serial}/render", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.bind))))
}

// triggerCfg mirrors the supervisor's parsed trigger_config shape (buildframe.go) — validated here at the
// edge so a malformed config is a 422, never a runtime surprise on the render path.
type triggerCfg struct {
	Mode      string `json:"mode"`       // render: "" | "sync" | "prerender"
	TTLSec    int    `json:"ttl_s"`      // sync hot-cache TTL
	IntervalS int    `json:"interval_s"` // prerender/schedule cadence
	Dither    string `json:"dither"`
	Cron      string `json:"cron"` // schedule // TODO(cron): 5-field parse once the supervisor scheduler consumes it
}

// list — GET /api/functions (auth): the list-view projection (no source, no config, NO token hash, K8).
func (h faasHandlers) list(w http.ResponseWriter, r *http.Request) {
	sums, err := faasstore.ListFunctions(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"functions": sums})
}

// get — GET /api/functions/{id} (auth): the full function (source + config + bindings) plus the serials
// bound to it. The webhook token hash is NOT on Function (K8) so it can never be echoed here.
func (h faasHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := faasID(w, r)
	if !ok {
		return
	}
	fn, err := faasstore.LoadFunction(r.Context(), h.pool, id)
	if errors.Is(err, faasstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function load failed")
		return
	}
	serials, err := faasstore.BoundSerials(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "bound-serial lookup failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"function": fnFields(fn), "bound_serials": serials})
}

// create — POST /api/functions (admin): create a function (enabled=false default). Name unique → 409;
// validation before the DB → 422. For a webhook trigger the server GENERATES the token, stores only its
// sha256, and returns the plaintext ONCE in this response (D24.13/D17.2) — the only time it is visible.
func (h faasHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string          `json:"name"`
		Source         string          `json:"source"`
		TriggerType    string          `json:"trigger_type"`
		TriggerConfig  json.RawMessage `json:"trigger_config"`
		SecretBindings []string        `json:"secret_bindings"`
		EgressAllow    []string        `json:"egress_allow"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if !faasstore.ValidName(body.Name) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_name",
			`name must match ^[a-z0-9][a-z0-9._-]{0,127}$`)
		return
	}
	tt, ok := triggerTypeOrDefault(body.TriggerType)
	if !ok {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_trigger_type",
			"trigger_type must be render|schedule|webhook")
		return
	}
	if body.Source == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_source", "source is required")
		return
	}
	if code, msg, ok := validTriggerConfig(tt, body.TriggerConfig); !ok {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, code, msg)
		return
	}

	var plaintext string
	var sha []byte
	if tt == faasstore.TriggerWebhook {
		plaintext, sha = genWebhookToken()
	}
	id, err := faasstore.Create(r.Context(), h.pool, faasstore.CreateParams{
		Name: body.Name, Source: body.Source, TriggerType: tt, TriggerConfig: body.TriggerConfig,
		SecretBindings: body.SecretBindings, EgressAllow: body.EgressAllow, WebhookTokenSHA: sha,
	})
	if err != nil {
		if faasstore.IsUniqueViolation(err) {
			adminhttp.WriteErr(w, r, http.StatusConflict, "duplicate_name", "a function with that name already exists")
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function create failed")
		return
	}
	fields := map[string]any{"id": id, "name": body.Name, "trigger_type": tt, "enabled": false}
	if plaintext != "" {
		fields["webhook_token"] = plaintext // shown ONCE — never recoverable, rotate to replace (D24.13)
	}
	adminhttp.WriteOK(w, r, fields)
}

// update — PUT /api/functions/{id} (admin): replace source/config/bindings/egress and BUMP version
// (K7 cache-bust). trigger_type and the webhook token are immutable here (rotation is PATCH).
func (h faasHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := faasID(w, r)
	if !ok {
		return
	}
	var body struct {
		Source         string          `json:"source"`
		TriggerConfig  json.RawMessage `json:"trigger_config"`
		SecretBindings []string        `json:"secret_bindings"`
		EgressAllow    []string        `json:"egress_allow"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	// load first so trigger_config is validated against the function's actual trigger_type (and 404 early).
	fn, err := faasstore.LoadFunction(r.Context(), h.pool, id)
	if errors.Is(err, faasstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function load failed")
		return
	}
	if body.Source == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_source", "source is required")
		return
	}
	if code, msg, ok := validTriggerConfig(fn.TriggerType, body.TriggerConfig); !ok {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, code, msg)
		return
	}
	found, err := faasstore.Update(r.Context(), h.pool, id, faasstore.UpdateParams{
		Source: body.Source, TriggerConfig: body.TriggerConfig,
		SecretBindings: body.SecretBindings, EgressAllow: body.EgressAllow,
	})
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function update failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "updated": true})
}

// patch — PATCH /api/functions/{id} (admin): toggle enabled (pausability) and/or rotate the webhook
// token. rotate_token is valid only on a webhook function; on rotation the new plaintext is returned
// ONCE (D24.13), same one-time-visibility contract as create. At least one field must be present.
func (h faasHandlers) patch(w http.ResponseWriter, r *http.Request) {
	id, ok := faasID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled     *bool `json:"enabled"`
		RotateToken bool  `json:"rotate_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Enabled == nil && !body.RotateToken {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "no_change",
			"provide enabled and/or rotate_token")
		return
	}
	fields := map[string]any{"id": id}
	if body.Enabled != nil {
		found, err := faasstore.SetEnabled(r.Context(), h.pool, id, *body.Enabled)
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "set enabled failed")
			return
		}
		if !found {
			adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
			return
		}
		fields["enabled"] = *body.Enabled
	}
	if body.RotateToken {
		fn, err := faasstore.LoadFunction(r.Context(), h.pool, id)
		if errors.Is(err, faasstore.ErrNotFound) {
			adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
			return
		}
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function load failed")
			return
		}
		if fn.TriggerType != faasstore.TriggerWebhook {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "not_webhook",
				"only a webhook function has a token to rotate")
			return
		}
		plaintext, sha := genWebhookToken()
		if _, err := faasstore.SetWebhookToken(r.Context(), h.pool, id, sha); err != nil {
			adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "token rotate failed")
			return
		}
		fields["webhook_token"] = plaintext // shown ONCE
	}
	adminhttp.WriteOK(w, r, fields)
}

// delete — DELETE /api/functions/{id} (admin): the FK CASCADE drops its device bindings + last-good rows.
func (h faasHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := faasID(w, r)
	if !ok {
		return
	}
	found, err := faasstore.Delete(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "function delete failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such function")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "deleted": true})
}

// bind — PUT /api/devices/{serial}/render (admin): point a serial at a function (exactly one per serial).
// device_render_binding.serial has NO FK to devices (0009: a binding may name a to-be-re-onboarded serial)
// so the device existence is checked at the edge → 422; an unknown function_id trips the FK → 422.
func (h faasHandlers) bind(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	var body struct {
		FunctionID int64 `json:"function_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.FunctionID <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_function_id", "function_id is required")
		return
	}
	var exists bool
	if err := h.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM devices WHERE serial = $1)`, serial).Scan(&exists); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "device check failed")
		return
	}
	if !exists {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_serial", "no such device")
		return
	}
	if err := faasstore.BindDevice(r.Context(), h.pool, serial, body.FunctionID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // FK → unknown function_id
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_function", "function_id does not exist")
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "bind failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": serial, "function_id": body.FunctionID})
}

// --- helpers ---

// fnFields is the full-function response shape (snake_case, matching the admin envelope). It deliberately
// carries NO token hash — the Function struct does not hold one (K8), so no drift can ever echo it.
func fnFields(fn *faasstore.Function) map[string]any {
	return map[string]any{
		"id": fn.ID, "name": fn.Name, "source": fn.Source, "version": fn.Version,
		"trigger_type": fn.TriggerType, "trigger_config": fn.TriggerConfig,
		"secret_bindings": fn.SecretBindings, "egress_allow": fn.EgressAllow,
		"enabled": fn.Enabled, "created_at": fn.CreatedAt, "updated_at": fn.UpdatedAt,
	}
}

func triggerTypeOrDefault(s string) (faasstore.TriggerType, bool) {
	switch s {
	case "":
		return faasstore.TriggerRender, true // 0009 default
	case string(faasstore.TriggerRender), string(faasstore.TriggerSchedule), string(faasstore.TriggerWebhook):
		return faasstore.TriggerType(s), true
	default:
		return "", false
	}
}

// validTriggerConfig checks the config parses into the known shape and its cadence fields are sane. An
// empty config is the {} default. It returns (code, msg, ok) so the caller writes a specific 422.
func validTriggerConfig(tt faasstore.TriggerType, raw json.RawMessage) (code, msg string, ok bool) {
	if len(raw) == 0 {
		return "", "", true
	}
	var c triggerCfg
	if err := json.Unmarshal(raw, &c); err != nil {
		return "invalid_trigger_config", "trigger_config must be a JSON object matching the trigger shape", false
	}
	if c.TTLSec < 0 || c.IntervalS < 0 {
		return "invalid_trigger_config", "ttl_s and interval_s must be >= 0", false
	}
	if tt == faasstore.TriggerRender && c.Mode != "" && c.Mode != "sync" && c.Mode != "prerender" {
		return "invalid_trigger_config", `render mode must be "sync" or "prerender"`, false
	}
	// TODO(cron): validate the 5-field cron string for schedule triggers once the supervisor scheduler
	// parses it (faas-supervisor tickSchedule uses interval_s in M1, so cron is a documented no-op here).
	return "", "", true
}

// genWebhookToken mints a random token, returns the plaintext (shown once) + its sha256 (stored). The
// hash is always 32 bytes → satisfies the 0009 faas_webhook_token_len CHECK regardless of token length.
func genWebhookToken() (plaintext string, sha []byte) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	plaintext = hex.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(plaintext))
	return plaintext, sum[:]
}

// faasID parses {id} and writes a 422 (+ returns false) on a non-integer / non-positive path value.
func faasID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_id", "function id must be a positive integer")
		return 0, false
	}
	return id, true
}
