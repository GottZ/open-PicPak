package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/playliststore"
)

// --- DB property tests for the A28 W5b playlist CRUD + binding routes (skipped unless TEST_DATABASE_URL) ---

func testPlaylistHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerPlaylistRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// seedPlaylist creates a playlist through the PRODUCTION store write path (not a raw INSERT) and returns
// its id — fixtures ride the same code the handler does.
func seedPlaylist(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	pl, err := playliststore.CreatePlaylist(context.Background(), pool, playliststore.CreateParams{Name: name})
	if err != nil {
		t.Fatalf("seed playlist %q: %v", name, err)
	}
	return pl.ID
}

// seedImage stores a distinct image through the production imgstore path and returns its id.
func seedImage(t *testing.T, pool *pgxpool.Pool, blobDir string, w, h int) int64 {
	t.Helper()
	img, err := imgstore.PutImage(context.Background(), pool, blobDir, nil, pngBytes(t, w, h))
	if err != nil {
		t.Fatalf("seed image %dx%d: %v", w, h, err)
	}
	return img.ID
}

func playlistVersion(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(), `SELECT version FROM playlist WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

// TestPlaylistGating_DB — the auth probes. Writes gate on image:write, reads on image:read, and the
// device→playlist bind on RequireAdmin. Red (a route without its middleware): the read-only token reaches
// a mutation, or a non-admin key reaches the bind.
func TestPlaylistGating_DB(t *testing.T) {
	pool := dbPool(t)
	readTok := mintScoped(t, pool, "ro", []string{adminhttp.ScopeImageRead})
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-op", false)
	h := testPlaylistHandler(pool)

	// POST /api/playlists (image:write gate)
	if w := do(h, "POST", "/api/playlists", "", strings.NewReader(`{"name":"g1"}`), "application/json"); w.Code != http.StatusUnauthorized {
		t.Errorf("POST no credential = %d, want 401", w.Code)
	}
	if w := do(h, "POST", "/api/playlists", readTok, strings.NewReader(`{"name":"g1"}`), "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("POST image:read-only = %d, want 403", w.Code)
	}
	if w := do(h, "POST", "/api/playlists", writeTok, strings.NewReader(`{"name":"g1"}`), "application/json"); w.Code != http.StatusCreated {
		t.Errorf("POST image:write = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	// GET /api/playlists (image:read gate)
	if w := do(h, "GET", "/api/playlists", writeTok, nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("GET list image:write-only = %d, want 403 (lacks read scope)", w.Code)
	}
	if w := do(h, "GET", "/api/playlists", readTok, nil, ""); w.Code != http.StatusOK {
		t.Errorf("GET list image:read = %d, want 200", w.Code)
	}

	// PUT /api/devices/{serial}/playlist (admin gate) — an image:write token must NOT reach fleet control.
	if w := do(h, "PUT", "/api/devices/gsn/playlist", "", strings.NewReader(`{"playlist_id":1}`), "application/json"); w.Code != http.StatusUnauthorized {
		t.Errorf("PUT bind no credential = %d, want 401", w.Code)
	}
	if w := do(h, "PUT", "/api/devices/gsn/playlist", writeTok, strings.NewReader(`{"playlist_id":1}`), "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("PUT bind image:write api_token = %d, want 403 (not admin)", w.Code)
	}
	if w := do(h, "PUT", "/api/devices/gsn/playlist", "ro-op", strings.NewReader(`{"playlist_id":1}`), "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("PUT bind non-admin operator = %d, want 403", w.Code)
	}
	// admin key passes the gate (422 unknown_serial past it, not 401/403).
	if w := do(h, "PUT", "/api/devices/gsn/playlist", "admin-tok", strings.NewReader(`{"playlist_id":1}`), "application/json"); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("PUT bind admin = %d, want past the gate", w.Code)
	}
}

// TestPlaylistReorder_DB — a full-order reorder atomically renumbers positions; an incomplete or
// duplicate order is a 422 (rot: a naive route accepts the partial order and corrupts the sequence).
func TestPlaylistReorder_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	blobDir := t.TempDir()
	h := testPlaylistHandler(pool)

	plID := seedPlaylist(t, pool, "reorder")
	var itemIDs []int64
	for i, dim := range []int{4, 5, 6} {
		imgID := seedImage(t, pool, blobDir, dim, dim)
		it, err := playliststore.AddItem(context.Background(), pool, plID, playliststore.AddItemParams{ImageID: imgID})
		if err != nil {
			t.Fatalf("add item %d: %v", i, err)
		}
		itemIDs = append(itemIDs, it.ID)
	}

	// Reverse the order via PATCH items.
	rev := []int64{itemIDs[2], itemIDs[1], itemIDs[0]}
	body := `{"item_ids":[` + strconv.FormatInt(rev[0], 10) + `,` + strconv.FormatInt(rev[1], 10) + `,` + strconv.FormatInt(rev[2], 10) + `]}`
	if w := do(h, "PATCH", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(body), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("reorder = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	// Assert the stored position sequence is exactly the reversed id order.
	rows, err := pool.Query(context.Background(), `SELECT id FROM playlist_item WHERE playlist_id = $1 ORDER BY position`, plID)
	if err != nil {
		t.Fatalf("read order: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if len(got) != 3 || got[0] != rev[0] || got[1] != rev[1] || got[2] != rev[2] {
		t.Fatalf("position order = %v, want %v", got, rev)
	}

	// Incomplete order (missing one id) → 422, sequence untouched.
	inc := `{"item_ids":[` + strconv.FormatInt(itemIDs[0], 10) + `,` + strconv.FormatInt(itemIDs[1], 10) + `]}`
	if w := do(h, "PATCH", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(inc), "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("incomplete reorder = %d, want 422", w.Code)
	}
	// Duplicate id → 422.
	dup := `{"item_ids":[` + strconv.FormatInt(itemIDs[0], 10) + `,` + strconv.FormatInt(itemIDs[0], 10) + `,` + strconv.FormatInt(itemIDs[1], 10) + `]}`
	if w := do(h, "PATCH", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(dup), "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate reorder = %d, want 422", w.Code)
	}
}

// TestPlaylistVersionBump_DB — version bumps on EVERY playlist/item mutation (the 0012 selection
// cache-bust invariant). Red (a mutation path that skips the bump): a device would serve a stale frame.
func TestPlaylistVersionBump_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	blobDir := t.TempDir()
	h := testPlaylistHandler(pool)

	// create → version 1
	cw := do(h, "POST", "/api/playlists", writeTok, strings.NewReader(`{"name":"bump"}`), "application/json")
	if cw.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", cw.Code, cw.Body.String())
	}
	plID := int64(jsonBody(t, cw)["playlist"].(map[string]any)["id"].(float64))
	prev := playlistVersion(t, pool, plID)
	if prev != 1 {
		t.Fatalf("fresh version = %d, want 1", prev)
	}

	imgID := seedImage(t, pool, blobDir, 7, 7)
	step := func(label, method, path, body string) {
		if w := do(h, method, path, writeTok, strings.NewReader(body), "application/json"); w.Code >= 300 {
			t.Fatalf("%s = %d (%s)", label, w.Code, w.Body.String())
		}
		if v := playlistVersion(t, pool, plID); v <= prev {
			t.Errorf("%s: version %d did not bump above %d", label, v, prev)
		} else {
			prev = v
		}
	}

	step("add item", "POST", "/api/playlists/"+itoa(plID)+"/items", `{"image_id":`+itoa(imgID)+`}`)
	// grab the item id for remove + reorder
	var itemID int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM playlist_item WHERE playlist_id=$1`, plID).Scan(&itemID); err != nil {
		t.Fatalf("read item id: %v", err)
	}
	step("reorder", "PATCH", "/api/playlists/"+itoa(plID)+"/items", `{"item_ids":[`+itoa(itemID)+`]}`)
	step("update policy", "PATCH", "/api/playlists/"+itoa(plID), `{"interval_s":120,"order_mode":"shuffle"}`)
	step("rename", "PATCH", "/api/playlists/"+itoa(plID), `{"name":"bump2"}`)
	step("reshuffle", "POST", "/api/playlists/"+itoa(plID)+"/reshuffle", `{}`)
	step("remove item", "DELETE", "/api/playlists/"+itoa(plID)+"/items/"+itoa(itemID), ``)
}

// TestPlaylistBindMutualExclusion_DB — binding a playlist to a serial that already holds a FUNCTION
// binding takes it over: afterward exactly ONE binding remains (playlist), the fn binding is gone — no
// double binding (design 27 §3/§5). Unbind then clears both the binding and the rotation cursor.
func TestPlaylistBindMutualExclusion_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testPlaylistHandler(pool)
	ctx := context.Background()

	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('mx-sn','stable')`)
	plID := seedPlaylist(t, pool, "mx")
	// Give the serial a function binding first (the production store path).
	fnID, err := faasstore.Create(ctx, pool, faasstore.CreateParams{Name: "mx-fn", Source: "y"})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, "mx-sn", fnID); err != nil {
		t.Fatalf("bind fn: %v", err)
	}

	// Bind the playlist — should take over the fn binding.
	if w := do(h, "PUT", "/api/devices/mx-sn/playlist", "admin-tok", strings.NewReader(`{"playlist_id":`+itoa(plID)+`}`), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("bind playlist = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var fnBindings, plBindings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_render_binding WHERE serial='mx-sn'`).Scan(&fnBindings); err != nil {
		t.Fatalf("count fn bindings: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_playlist_binding WHERE serial='mx-sn'`).Scan(&plBindings); err != nil {
		t.Fatalf("count pl bindings: %v", err)
	}
	if fnBindings != 0 {
		t.Errorf("device_render_binding count = %d, want 0 (taken over — no double binding)", fnBindings)
	}
	if plBindings != 1 {
		t.Errorf("device_playlist_binding count = %d, want 1", plBindings)
	}

	// Plant a cursor row, then unbind → both the binding and the cursor must be gone.
	mustExec(t, pool, `INSERT INTO playlist_cursor (serial, playlist_id, position) VALUES ('mx-sn',$1,2)`, plID)
	if w := do(h, "DELETE", "/api/devices/mx-sn/playlist", "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("unbind = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var cur, bind int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM playlist_cursor WHERE serial='mx-sn'`).Scan(&cur); err != nil {
		t.Fatalf("count cursor: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_playlist_binding WHERE serial='mx-sn'`).Scan(&bind); err != nil {
		t.Fatalf("count binding: %v", err)
	}
	if cur != 0 {
		t.Errorf("cursor rows after unbind = %d, want 0 (unbind must clear the cursor)", cur)
	}
	if bind != 0 {
		t.Errorf("binding rows after unbind = %d, want 0", bind)
	}
	// A second unbind is a 404 (no binding left).
	if w := do(h, "DELETE", "/api/devices/mx-sn/playlist", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("second unbind = %d, want 404", w.Code)
	}
}

// TestPlaylistDeleteInUse_DB — deleting a playlist bound to a device is a 409 (in-use policy); once
// unbound it deletes. Red (a naive route calling DeletePlaylist directly): the cascade silently blanks the
// bound panel and orphans its cursor.
func TestPlaylistDeleteInUse_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	seedOperator(t, pool, "admin-tok", true)
	h := testPlaylistHandler(pool)

	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('del-sn','stable')`)
	plID := seedPlaylist(t, pool, "delme")
	if w := do(h, "PUT", "/api/devices/del-sn/playlist", "admin-tok", strings.NewReader(`{"playlist_id":`+itoa(plID)+`}`), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("bind = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "DELETE", "/api/playlists/"+itoa(plID), writeTok, nil, ""); w.Code != http.StatusConflict {
		t.Errorf("delete bound playlist = %d, want 409", w.Code)
	}
	if w := do(h, "DELETE", "/api/devices/del-sn/playlist", "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("unbind = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "DELETE", "/api/playlists/"+itoa(plID), writeTok, nil, ""); w.Code != http.StatusOK {
		t.Errorf("delete unbound playlist = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

// TestPlaylistAddItemErrors_DB — an unknown image_id is a clean 422 (not a 500 leaking the FK), and a bad
// fit is a 422 before the store's DB CHECK.
func TestPlaylistAddItemErrors_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	blobDir := t.TempDir()
	h := testPlaylistHandler(pool)
	plID := seedPlaylist(t, pool, "items")

	if w := do(h, "POST", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(`{"image_id":999999}`), "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("add unknown image = %d, want 422", w.Code)
	}
	if b := jsonBody(t, do(h, "POST", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(`{"image_id":999999}`), "application/json")); b["code"] != "unknown_image" {
		t.Errorf("unknown image code = %v, want unknown_image", b["code"])
	}

	imgID := seedImage(t, pool, blobDir, 8, 8)
	if w := do(h, "POST", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(`{"image_id":`+itoa(imgID)+`,"fit":"wut"}`), "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("add bad fit = %d, want 422", w.Code)
	}
	// unknown playlist → 404
	if w := do(h, "POST", "/api/playlists/999999/items", writeTok, strings.NewReader(`{"image_id":`+itoa(imgID)+`}`), "application/json"); w.Code != http.StatusNotFound {
		t.Errorf("add to unknown playlist = %d, want 404", w.Code)
	}
	// the valid add still works → 201
	if w := do(h, "POST", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(`{"image_id":`+itoa(imgID)+`}`), "application/json"); w.Code != http.StatusCreated {
		t.Errorf("add valid item = %d, want 201 (%s)", w.Code, w.Body.String())
	}
}

// TestPlaylistNotify_DB — NotifyPlaylistChanged fires on a mutation AND on a bind (the pre-pack-warmer
// wiring seam, §4.6). A dedicated LISTEN connection asserts the pg_notify('playlist_changed', <id>)
// reaches it. Red (a handler that skips the notify): the warmer never wakes and the first device hits a
// cold variant.
func TestPlaylistNotify_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	seedOperator(t, pool, "admin-tok", true)
	blobDir := t.TempDir()
	h := testPlaylistHandler(pool)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire listen conn: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN playlist_changed"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	plID := seedPlaylist(t, pool, "notify")
	imgID := seedImage(t, pool, blobDir, 9, 9)

	waitNotify := func(label string) string {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("%s: no playlist_changed notification: %v", label, err)
		}
		if n.Channel != "playlist_changed" {
			t.Fatalf("%s: channel = %q, want playlist_changed", label, n.Channel)
		}
		return n.Payload
	}

	// Mutation → notify with the playlist id as payload.
	if w := do(h, "POST", "/api/playlists/"+itoa(plID)+"/items", writeTok, strings.NewReader(`{"image_id":`+itoa(imgID)+`}`), "application/json"); w.Code != http.StatusCreated {
		t.Fatalf("add item = %d (%s)", w.Code, w.Body.String())
	}
	if p := waitNotify("add item"); p != strconv.FormatInt(plID, 10) {
		t.Errorf("notify payload = %q, want %d", p, plID)
	}

	// Bind → notify too (warm before wake).
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('nf-sn','stable')`)
	if w := do(h, "PUT", "/api/devices/nf-sn/playlist", "admin-tok", strings.NewReader(`{"playlist_id":`+itoa(plID)+`}`), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("bind = %d (%s)", w.Code, w.Body.String())
	}
	if p := waitNotify("bind"); p != strconv.FormatInt(plID, 10) {
		t.Errorf("bind notify payload = %q, want %d", p, plID)
	}
}
