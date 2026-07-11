package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/plrender"
)

// panelHandlers serves the "Aufs Panel" single-image shortcut (design/33 §4.5b, W-A33.7): the laien
// path "put THIS image on THAT panel" without authoring a playlist. It reuses the A27 plrender +
// playlist fabric — the shortcut is a managed single-image playlist keyed by the managed_serial column,
// bound mutually-exclusive with any function binding.
type panelHandlers struct{ pool *pgxpool.Pool }

// registerPanelRoutes mounts the shortcut route. Called from registerImageRoutes (its natural home —
// the trigger is an image card), so main.go stays untouched. Single wiring source, mirror of the
// sibling register* helpers.
//
// SCOPE: pointing a physical panel at content is FLEET control, not media authoring — RequireAdmin,
// the exact mirror of the device→function render bind and the device→playlist bind (playlist_http
// h.bind). A leaked image:* api_token can arrange images but never reaches this admin-gated bind.
func registerPanelRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := panelHandlers{pool: pool}
	mux.Handle("PUT /api/devices/{serial}/image",
		adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(h.setImage))))
}

// setImage — PUT /api/devices/{serial}/image (admin) {image_id}: point a serial at a single image. The
// whole mutation is atomic + mutually-exclusive with a function binding inside plrender.SetManagedImage
// (advisory-locked tx, A27-W3 pattern) — it clears any fn binding, upserts the one managed playlist for
// the serial (exactly one, no growth past the device count), sets its lone item, and binds the device.
// device_playlist_binding.serial has NO FK to devices (parity the render bind), so device existence is
// checked at the edge → 422; the image is pre-checked → clean 422 (disambiguating the item FK).
func (h panelHandlers) setImage(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	var body struct {
		ImageID int64 `json:"image_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.ImageID <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_image_id", "image_id is required")
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
	if _, err := imgstore.GetImage(r.Context(), h.pool, body.ImageID); errors.Is(err, imgstore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_image", "image_id does not exist")
		return
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "image read failed")
		return
	}
	plID, err := plrender.SetManagedImage(r.Context(), h.pool, serial, body.ImageID)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "set image failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": serial, "image_id": body.ImageID, "playlist_id": plID})
}
