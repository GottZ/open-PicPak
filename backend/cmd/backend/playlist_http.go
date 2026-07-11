package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/commandstore"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/playliststore"
	"github.com/open-picpak/backend/internal/plrender"
)

// playlistHandlers serves the A28 W5b playlist CRUD + device-binding surface (design 29 §Merge-Punkt
// A28 (f)/(g), design 27 §4.2/§4.3). It mounts on the A27 playliststore + plrender packages and reuses
// the media-surface envelope + auth exactly as image_http does.
type playlistHandlers struct{ pool *pgxpool.Pool }

// registerPlaylistRoutes mounts the playlist CRUD + item + device-bind routes (single wiring source, so
// main.go and the gating test can never drift — mirror of registerImageRoutes).
//
// SCOPE CHOICE (design 27/29 define no dedicated playlist scope). Playlists are the SAME media fabric as
// images ("Bild+Playlist" is one operator area, design 29 §1), so playlist reads/writes ride the SAME
// image:read / image:write scopes the sibling image surface already uses (registerImageRoutes) — a media
// operator token that may arrange images into playlists is exactly one that may manage images. The
// device→playlist BIND is fleet control, not media authoring: it points a physical panel at content, so
// it gates on RequireAdmin — the EXACT mirror of the device→function render bind (PUT/DELETE
// /api/devices/{serial}/render, faas_http/faas_ui). Session/basic/operator admins hold the image scopes
// implicitly, so a human always reaches the CRUD; a leaked image:* api_token never reaches the
// admin-gated bind.
func registerPlaylistRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := playlistHandlers{pool: pool}
	write := func(fn http.HandlerFunc) http.Handler {
		return adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageWrite)(fn))
	}
	read := func(fn http.HandlerFunc) http.Handler {
		return adminhttp.Auth(pool)(adminhttp.RequireScope(adminhttp.ScopeImageRead)(fn))
	}
	admin := func(fn http.HandlerFunc) http.Handler {
		return adminhttp.Auth(pool)(adminhttp.RequireAdmin(fn))
	}

	mux.Handle("POST /api/playlists", write(h.create))
	mux.Handle("GET /api/playlists", read(h.list))
	mux.Handle("GET /api/playlists/{id}", read(h.get))
	mux.Handle("PATCH /api/playlists/{id}", write(h.update))
	mux.Handle("DELETE /api/playlists/{id}", write(h.delete))
	mux.Handle("POST /api/playlists/{id}/reshuffle", write(h.reshuffle))
	// The item routes are more specific than /api/playlists/{id}; Go 1.22 routing prefers them, so the
	// PATCH-items reorder never collides with the PATCH-playlist update.
	mux.Handle("POST /api/playlists/{id}/items", write(h.addItem))
	mux.Handle("PATCH /api/playlists/{id}/items", write(h.reorder))
	mux.Handle("DELETE /api/playlists/{id}/items/{itemId}", write(h.removeItem))

	// Device→playlist binding — fleet control, admin-gated (mirror of the render bind).
	mux.Handle("PUT /api/devices/{serial}/playlist", admin(h.bind))
	mux.Handle("DELETE /api/devices/{serial}/playlist", admin(h.unbind))
}

// notify fires the pre-pack-warmer signal (27:§4.6) after a committed mutation/bind. Fire-and-forget: the
// mutation already persisted, and NotifyPlaylistChanged is a no-op when no warmer listens, so a notify
// error must never fail the request — the /frame single-flight is the correctness backstop, a lost signal
// costs at most one cold render, never a wrong frame.
func (h playlistHandlers) notify(ctx context.Context, playlistID int64) {
	_ = commandstore.NotifyPlaylistChanged(ctx, h.pool, playlistID)
}

// create — POST /api/playlists (image:write): create a playlist (name + optional order_mode/interval_s).
// Name + policy are validated Go-side (clean 422 before the DB CHECKs); a duplicate name is a 409. The
// operator_key attribution rides mintedBy (only an operator carrier has an operator_keys id; a
// session/basic admin stores NULL — the true actor is still on the request).
func (h playlistHandlers) create(w http.ResponseWriter, r *http.Request) {
	pr, _ := adminhttp.PrincipalFrom(r.Context())
	var body struct {
		Name      string `json:"name"`
		OrderMode string `json:"order_mode"`
		IntervalS int    `json:"interval_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	pl, err := playliststore.CreatePlaylist(r.Context(), h.pool, playliststore.CreateParams{
		OperatorKeyID: mintedBy(pr), Name: body.Name, OrderMode: body.OrderMode, IntervalS: body.IntervalS,
	})
	switch {
	case errors.Is(err, playliststore.ErrNameInvalid):
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_name",
			"name must match ^[a-z0-9][a-z0-9._-]{0,127}$")
		return
	case errors.Is(err, playliststore.ErrPolicyInvalid):
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_policy",
			"order_mode must be sequential|shuffle")
		return
	case playliststore.IsUniqueViolation(err):
		adminhttp.WriteErr(w, r, http.StatusConflict, "name_conflict", "a playlist with that name already exists")
		return
	case err != nil:
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist create failed")
		return
	}
	h.notify(r.Context(), pl.ID)
	adminhttp.WriteCreated(w, r, map[string]any{"playlist": pl})
}

// list — GET /api/playlists (image:read): a keyset page over the store, ordered by id (WHERE id > after).
// Managed ("Aufs Panel", Delta 3) rows are hidden by default — an operator list is not flooded with
// per-serial management noise. next_cursor is the last id on a full page (more may follow), else null.
func (h playlistHandlers) list(w http.ResponseWriter, r *http.Request) {
	limit := clampPlaylistLimit(r.URL.Query().Get("limit"))
	cursor := parsePlaylistCursor(r.URL.Query().Get("after"))
	rows, err := playliststore.ListPlaylists(r.Context(), h.pool, limit, cursor, false)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist list failed")
		return
	}
	var next any
	if len(rows) == limit && len(rows) > 0 {
		next = rows[len(rows)-1].ID
	}
	adminhttp.WriteOK(w, r, map[string]any{"playlists": rows, "next_cursor": next})
}

// get — GET /api/playlists/{id} (image:read): the playlist plus a keyset page of its items ordered by
// position. Items are paginated (never the whole list in one shot) because playlist_item is the
// highest-cardinality entity at target scale — the editor loads pages (design 27 §4.2/§6). Pass
// ?after=<position> for the next page; next_cursor is the last position on a full page. Items carry the
// image_id reference (the editor builds thumbnail/frame URLs from it — the 30000 B frame is never
// embedded in JSON); the image-metadata join is deferred (report open point).
func (h playlistHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	pl, err := playliststore.GetPlaylist(r.Context(), h.pool, id)
	if errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	limit := clampPlaylistLimit(r.URL.Query().Get("limit"))
	items, err := playliststore.ListItems(r.Context(), h.pool, id, limit, parseItemCursor(r.URL.Query().Get("after")))
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist items read failed")
		return
	}
	var next any
	if len(items) == limit && len(items) > 0 {
		next = items[len(items)-1].Position
	}
	adminhttp.WriteOK(w, r, map[string]any{"playlist": pl, "items": items, "next_cursor": next})
}

// update — PATCH /api/playlists/{id} (image:write): a partial update of name and/or rotation policy
// (interval_s / order_mode). At least one field is required. Every update bumps version (0012 invariant);
// a duplicate name is a 409, a bad value a 422.
func (h playlistHandlers) update(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		Name      *string `json:"name"`
		OrderMode *string `json:"order_mode"`
		IntervalS *int    `json:"interval_s"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.Name == nil && body.OrderMode == nil && body.IntervalS == nil {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "nothing_to_update",
			"provide name and/or order_mode and/or interval_s")
		return
	}
	pl, err := playliststore.UpdatePlaylist(r.Context(), h.pool, id, playliststore.UpdateParams{
		Name: body.Name, IntervalS: body.IntervalS, OrderMode: body.OrderMode,
	})
	switch {
	case errors.Is(err, playliststore.ErrNameInvalid):
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_name",
			"name must match ^[a-z0-9][a-z0-9._-]{0,127}$")
		return
	case errors.Is(err, playliststore.ErrPolicyInvalid):
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_policy",
			"order_mode must be sequential|shuffle and interval_s must be > 0")
		return
	case playliststore.IsUniqueViolation(err):
		adminhttp.WriteErr(w, r, http.StatusConflict, "name_conflict", "a playlist with that name already exists")
		return
	case errors.Is(err, playliststore.ErrNotFound):
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return
	case err != nil:
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist update failed")
		return
	}
	h.notify(r.Context(), id)
	adminhttp.WriteOK(w, r, map[string]any{"playlist": pl})
}

// reshuffle — POST /api/playlists/{id}/reshuffle (image:write): the explicit "reshuffle now" operator
// command (design 27 §4.4). It bumps shuffle_epoch (the ONLY action that does — item mutations never
// touch it, so a shuffle order is stable across restarts/edits until this is called) + version.
func (h playlistHandlers) reshuffle(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	err := playliststore.Reshuffle(r.Context(), h.pool, id)
	if errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "reshuffle failed")
		return
	}
	h.notify(r.Context(), id)
	adminhttp.WriteOK(w, r, map[string]any{"reshuffled": id})
}

// delete — DELETE /api/playlists/{id} (image:write). IN-USE POLICY (design 27 §K2 / §5, documented
// deviation): the 0013 schema authored device_playlist_binding ON DELETE CASCADE, but this handler adds a
// 409-when-bound guard rather than exposing a silent cascade. Rationale: (a) mirrors the image-delete
// in-use 409 on the SAME media surface, (b) deleting a playlist bound to hundreds of panels would silently
// blank them, (c) the cascade also orphans the FK-LESS playlist_cursor rows. The operator must unbind
// first. The schema CASCADE remains the DB safety net for the tiny TOCTOU window (a bind landing between
// the count and the delete). Items go with the playlist (playlist_item ON DELETE CASCADE) once unbound.
func (h playlistHandlers) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	n, err := plrender.CountBindings(r.Context(), h.pool, id)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist binding check failed")
		return
	}
	if n > 0 {
		adminhttp.WriteErr(w, r, http.StatusConflict, "playlist_in_use",
			"playlist is bound to "+strconv.Itoa(n)+" device(s); unbind before deleting")
		return
	}
	err = playliststore.DeletePlaylist(r.Context(), h.pool, id)
	if errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist delete failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"deleted": id})
}

// addItem — POST /api/playlists/{id}/items (image:write): append an image to the END of a playlist.
// fit/dither are validated Go-side (clean 422). The playlist existence is pre-checked so the AddItem FK
// violation is UNAMBIGUOUSLY a missing image_id (both playlist_id and image_id are FKs) → 422 unknown_image.
func (h playlistHandlers) addItem(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		ImageID int64  `json:"image_id"`
		Fit     string `json:"fit"`
		Dither  string `json:"dither"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.ImageID <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_image_id", "image_id is required")
		return
	}
	if !playliststore.ValidFit(body.Fit) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_fit", "fit must be cover|contain|fill")
		return
	}
	if !playliststore.ValidDither(body.Dither) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_dither",
			"dither must be none|floyd-steinberg|atkinson|ordered")
		return
	}
	if _, err := playliststore.GetPlaylist(r.Context(), h.pool, id); errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	item, err := playliststore.AddItem(r.Context(), h.pool, id, playliststore.AddItemParams{
		ImageID: body.ImageID, Fit: body.Fit, Dither: body.Dither,
	})
	if playliststore.IsForeignKeyViolation(err) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_image", "image_id does not exist")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "add item failed")
		return
	}
	h.notify(r.Context(), id)
	adminhttp.WriteCreated(w, r, map[string]any{"item": item})
}

// removeItem — DELETE /api/playlists/{id}/items/{itemId} (image:write): drop one item and bump its
// playlist's version. Removal is by the globally-unique item id; the {id} segment is the REST parent for
// the notify. A missing item is a 404.
func (h playlistHandlers) removeItem(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	itemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such item")
		return
	}
	if err := playliststore.RemoveItem(r.Context(), h.pool, itemID); errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such item")
		return
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "remove item failed")
		return
	}
	h.notify(r.Context(), id)
	adminhttp.WriteOK(w, r, map[string]any{"removed": itemID})
}

// reorder — PATCH /api/playlists/{id}/items (image:write): a full-order reorder. item_ids MUST be exactly
// the playlist's current item set — a missing/foreign/duplicate id is a 422 (the two-stage renumber is
// rolled back atomically, never stranding rows), so a partial order can never corrupt the sequence.
func (h playlistHandlers) reorder(w http.ResponseWriter, r *http.Request) {
	id, ok := playlistID(w, r)
	if !ok {
		return
	}
	var body struct {
		ItemIDs []int64 `json:"item_ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	err := playliststore.Reorder(r.Context(), h.pool, id, body.ItemIDs)
	if errors.Is(err, playliststore.ErrReorderIncomplete) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "reorder_incomplete",
			"item_ids must be exactly the playlist's current items (no missing, foreign, or duplicate ids)")
		return
	}
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "reorder failed")
		return
	}
	h.notify(r.Context(), id)
	adminhttp.WriteOK(w, r, map[string]any{"reordered": id})
}

// bind — PUT /api/devices/{serial}/playlist (admin): point a serial at a playlist. MUTUAL EXCLUSION is by
// plrender.BindPlaylist's advisory-locked TAKEOVER (delete any device_render_binding + insert the playlist
// binding + reset the cursor), the exact mirror of faasstore.BindDevice — so a serial can never hold BOTH
// a function and a playlist binding (design 27 §3/§5). This follows the brief's "laut plrender-Semantik":
// plrender's actual mechanism is takeover, NOT a 409 (a 409 here would make the surface asymmetric — the
// render bind at faas_http silently takes over a playlist binding). device_playlist_binding.serial has NO
// FK to devices (parity device_render_binding), so device existence is checked at the edge → 422; an
// unknown playlist_id is a clean 422 (pre-checked, disambiguating the binding FK).
func (h playlistHandlers) bind(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	var body struct {
		PlaylistID int64 `json:"playlist_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		adminhttp.WriteErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON body")
		return
	}
	if body.PlaylistID <= 0 {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "missing_playlist_id", "playlist_id is required")
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
	if _, err := playliststore.GetPlaylist(r.Context(), h.pool, body.PlaylistID); errors.Is(err, playliststore.ErrNotFound) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "unknown_playlist", "playlist_id does not exist")
		return
	} else if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "playlist read failed")
		return
	}
	if err := plrender.BindPlaylist(r.Context(), h.pool, serial, body.PlaylistID); err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "bind failed")
		return
	}
	// Warm the newly-bound playlist's variants before the device wakes (§4.6).
	h.notify(r.Context(), body.PlaylistID)
	adminhttp.WriteOK(w, r, map[string]any{"serial": serial, "playlist_id": body.PlaylistID})
}

// unbind — DELETE /api/devices/{serial}/playlist (admin): drop a serial's playlist binding AND its
// rotation cursor (plrender.UnbindPlaylist clears both — a stale cursor must not survive an unbind). A
// serial with no playlist binding is a 404.
func (h playlistHandlers) unbind(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if !devicestore.ValidSerial(serial) {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial",
			`serial must match ^[A-Za-z0-9_-]{1,31}$`)
		return
	}
	existed, err := plrender.UnbindPlaylist(r.Context(), h.pool, serial)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "unbind failed")
		return
	}
	if !existed {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no playlist binding for this device")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serial": serial, "unbound": true})
}

// --- helpers ---

// playlistID parses {id} and writes a uniform 404 on a non-positive/non-integer path value (no
// enumeration signal, image_http parity).
func playlistID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such playlist")
		return 0, false
	}
	return id, true
}

// clampPlaylistLimit parses ?limit into [1,200] with a 50 default (keyset page bound, image_http parity).
func clampPlaylistLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// parsePlaylistCursor parses ?after into a non-negative id keyset cursor (0 = first page).
func parsePlaylistCursor(raw string) int64 {
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseItemCursor parses ?after into a position keyset cursor. Positions start at 0 and ListItems selects
// WHERE position > cursor, so the first-page default is -1 (an empty/invalid value returns -1, not 0 — a
// 0 would skip the item at position 0).
func parseItemCursor(raw string) int {
	if raw == "" {
		return -1
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return n
}
