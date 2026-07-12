package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/rollout"
	"github.com/open-picpak/backend/internal/rolloutadmin"
)

// maxFirmwareBytes caps an uploaded blob (ESP32-C3 app images are ~1-2 MB; 16 MB is generous headroom
// without inviting an OOM upload). Bounds both the multipart parse and the in-memory read.
const maxFirmwareBytes = 16 << 20

var (
	shaRe       = regexp.MustCompile(`^[0-9a-f]{64}$`) // the 0001:22 CHECK, pre-validated at the edge (→422)
	rolloutVals = map[string]bool{"active": true, "paused": true, "done": true}
)

type otaHandlers struct {
	pool    *pgxpool.Pool
	blobDir string
	// notify publishes the `ota` SSE reload-hint (eventsHandler.publishOTA, events.go — E2/A6).
	// Reached only through notifyOTA (nil-safe), so tests that do not observe events pass nil.
	notify func(kind string)
}

// registerOTARoutes mounts the 9 OTA serving/rollout-management routes (§4.4). Reads are auth-gated
// (any valid key); mutations require admin — the exact gating main.go applies to the device/secret
// routes. Single source of the wiring so main.go and the gating test (T9) can never drift. notify is
// the SSE reload-hint seam (E2/A6): every mutation handler publishes its collection kind after a
// successful write.
func registerOTARoutes(mux *http.ServeMux, pool *pgxpool.Pool, blobDir string, notify func(kind string)) {
	h := otaHandlers{pool: pool, blobDir: blobDir, notify: notify}
	mux.Handle("POST /api/firmware", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.registerFirmware))))
	mux.Handle("GET /api/firmware", adminhttp.Auth(pool)(http.HandlerFunc(h.listFirmware)))
	mux.Handle("GET /api/channels", adminhttp.Auth(pool)(http.HandlerFunc(h.listChannels)))
	mux.Handle("PUT /api/channels/{name}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.setChannelDefault))))
	mux.Handle("POST /api/rollouts", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.upsertRollout))))
	mux.Handle("PATCH /api/rollouts/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.setRolloutState))))
	mux.Handle("DELETE /api/rollouts/{id}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.deleteRollout))))
	mux.Handle("GET /api/rollouts", adminhttp.Auth(pool)(http.HandlerFunc(h.listRollouts)))
	mux.Handle("GET /api/resolve/{serial}", adminhttp.Auth(pool)(http.HandlerFunc(h.resolve)))
}

// notifyOTA fans the `ota` reload-hint after a SUCCESSFUL mutation — and only then: a refused or
// failed write must not make every connected panel re-fetch (E2/A6).
func (h otaHandlers) notifyOTA(kind string) {
	if h.notify != nil {
		h.notify(kind)
	}
}

// registerFirmware — POST /api/firmware (admin, multipart): version,sha256,blob. Validates format at
// the edge (→422), then rolloutadmin re-hashes the bytes and reserves the row crash-safe (§4.4.1).
func (h otaHandlers) registerFirmware(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFirmwareBytes+1<<20) // blob + form overhead
	if err := r.ParseMultipartForm(maxFirmwareBytes); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed or oversized multipart body")
		return
	}
	version := r.FormValue("version")
	claimedSha := strings.ToLower(r.FormValue("sha256"))
	if version == "" || len(version) > 31 { // 0001:20 version_len CHECK, pre-validated (→422 not 500)
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_version", "version must be 1..31 chars")
		return
	}
	if !shaRe.MatchString(claimedSha) { // T7 — 422 BEFORE the 0001:22 DB CHECK
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_sha", "sha256 must match ^[0-9a-f]{64}$")
		return
	}
	file, _, err := r.FormFile("blob")
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_blob", "multipart field 'blob' required")
		return
	}
	defer file.Close() //nolint:errcheck // read-only
	blob, err := io.ReadAll(io.LimitReader(file, maxFirmwareBytes+1))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "blob read failed")
		return
	}
	if len(blob) > maxFirmwareBytes {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "blob_too_large", "firmware blob exceeds the size cap")
		return
	}

	sha, err := rolloutadmin.RegisterFirmware(r.Context(), h.pool, h.blobDir, version, claimedSha, blob)
	if err != nil {
		if errors.Is(err, rolloutadmin.ErrShaMismatch) {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "sha_mismatch",
				"server-recomputed sha256 != claimed (nothing stored)")
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation on the version PK — no overwrite of an in-use version (T6)
				adminhttp.WriteErr(w, r, http.StatusConflict, "duplicate_version", "version already registered")
				return
			case "23514": // a CHECK slipped past the edge validation
				adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_version", "version/sha violates a constraint")
				return
			}
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "register failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"version": version, "sha256": sha, "size_bytes": len(blob)})
	h.notifyOTA(otaKindFirmware) // E2/A6: reload-hint after the successful register
}

func (h otaHandlers) listFirmware(w http.ResponseWriter, r *http.Request) {
	rows, err := rolloutadmin.ListFirmware(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "firmware list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"firmware": rows})
}

func (h otaHandlers) listChannels(w http.ResponseWriter, r *http.Request) {
	rows, err := rolloutadmin.ListChannels(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "channel list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"channels": rows})
}

// setChannelDefault — PUT /api/channels/{name} (admin): point the channel default at a version.
func (h otaHandlers) setChannelDefault(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Version == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_version", "version required")
		return
	}
	found, err := rolloutadmin.SetChannelDefault(r.Context(), h.pool, r.PathValue("name"), body.Version)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // FK → unknown version
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_version", "version does not exist")
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "set default failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such channel")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"channel": r.PathValue("name"), "default_version": body.Version})
	h.notifyOTA(otaKindChannel) // E2/A6
}

// upsertRollout — POST /api/rollouts (admin): upsert a per-serial pin or the '*' fleet rollout.
func (h otaHandlers) upsertRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial  string `json:"serial"`
		Channel string `json:"channel"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Serial != "*" && !devicestore.ValidSerial(body.Serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must be '*' or match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	if body.Channel == "" || body.Version == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_field", "channel and version required")
		return
	}
	// rollout_targets.serial has no FK (§4.4) → check existence at the edge (→422), not an orphan row.
	ok, err := rolloutadmin.SerialResolvable(r.Context(), h.pool, body.Serial)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "serial check failed")
		return
	}
	if !ok {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_serial", "no such device")
		return
	}
	id, err := rolloutadmin.UpsertRollout(r.Context(), h.pool, body.Serial, body.Channel, body.Version)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // FK → unknown channel or version
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_channel_or_version",
				"channel or version does not exist")
			return
		}
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "rollout upsert failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "serial": body.Serial, "channel": body.Channel, "version": body.Version})
	h.notifyOTA(otaKindRollout) // E2/A6
}

// setRolloutState — PATCH /api/rollouts/{id} (admin): active|paused|done.
func (h otaHandlers) setRolloutState(w http.ResponseWriter, r *http.Request) {
	id, ok := rolloutID(w, r)
	if !ok {
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if !rolloutVals[body.State] {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_state", "state must be active|paused|done")
		return
	}
	found, err := rolloutadmin.SetRolloutState(r.Context(), h.pool, id, body.State)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "set state failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such rollout")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "state": body.State})
	h.notifyOTA(otaKindRollout) // E2/A6
}

// deleteRollout — DELETE /api/rollouts/{id} (admin).
func (h otaHandlers) deleteRollout(w http.ResponseWriter, r *http.Request) {
	id, ok := rolloutID(w, r)
	if !ok {
		return
	}
	found, err := rolloutadmin.DeleteRollout(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "delete failed")
		return
	}
	if !found {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such rollout")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"id": id, "deleted": true})
	h.notifyOTA(otaKindRollout) // E2/A6
}

func (h otaHandlers) listRollouts(w http.ResponseWriter, r *http.Request) {
	rows, err := rolloutadmin.ListRollouts(r.Context(), h.pool)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "rollout list failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"rollouts": rows})
}

// resolve — GET /api/resolve/{serial} (auth): the AUTHORITATIVE resolved target via internal/rollout,
// the same resolver ingest serves on /pp — so the operator never sees a target the device never gets
// (D20.2/T12).
func (h otaHandlers) resolve(w http.ResponseWriter, r *http.Request) {
	res, err := rollout.ResolveTarget(r.Context(), h.pool, r.PathValue("serial"))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "resolve failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": r.PathValue("serial"), "resolved": res})
}

// rolloutID parses {id} and writes a 422 + returns false on a non-integer path value.
func rolloutID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_id", "rollout id must be an integer")
		return 0, false
	}
	return id, true
}
