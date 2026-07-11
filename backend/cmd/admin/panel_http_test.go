package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/playliststore"
)

// --- DB property tests for the "Aufs Panel" shortcut route (skipped unless TEST_DATABASE_URL) ---

const panelMaxBytes = 1 << 20

func countManaged(t *testing.T, pool *pgxpool.Pool, serial string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM playlist WHERE managed_serial = $1`, serial).Scan(&n); err != nil {
		t.Fatalf("count managed %s: %v", serial, err)
	}
	return n
}

// TestPanelGating_DB is the "Aufs Panel" auth probe (design/33 §4.5b, W-A33.7 probe d, server half):
// PUT /api/devices/{serial}/image is fleet control ⇒ RequireAdmin. An image:write api_token or a
// non-admin operator must be a 403; only an admin passes the gate. RED: the route without its
// RequireAdmin lets an image:write token reach a fleet mutation.
func TestPanelGating_DB(t *testing.T) {
	pool := dbPool(t)
	blobDir := t.TempDir()
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-op", false)
	h := testImageHandler(pool, blobDir, panelMaxBytes)

	if w := do(h, "PUT", "/api/devices/psn/image", "", strings.NewReader(`{"image_id":1}`), "application/json"); w.Code != http.StatusUnauthorized {
		t.Errorf("PUT no credential = %d, want 401", w.Code)
	}
	if w := do(h, "PUT", "/api/devices/psn/image", writeTok, strings.NewReader(`{"image_id":1}`), "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("PUT image:write api_token = %d, want 403 (not admin)", w.Code)
	}
	if w := do(h, "PUT", "/api/devices/psn/image", "ro-op", strings.NewReader(`{"image_id":1}`), "application/json"); w.Code != http.StatusForbidden {
		t.Errorf("PUT non-admin operator = %d, want 403", w.Code)
	}
	// admin passes the gate (422 unknown_serial past it — not 401/403).
	if w := do(h, "PUT", "/api/devices/psn/image", "admin-tok", strings.NewReader(`{"image_id":1}`), "application/json"); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("PUT admin = %d, want past the gate", w.Code)
	}
}

// TestPanelSetImage_DB drives the happy path end-to-end (admin PUT → managed playlist created, one item,
// bound, no fn binding) and the device-delete cleanup (path a): deleting the device reaps the serial's
// managed playlist so no orphan row survives.
func TestPanelSetImage_DB(t *testing.T) {
	pool := dbPool(t)
	blobDir := t.TempDir()
	seedOperator(t, pool, "admin-tok", true)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('psn','stable')`)
	imgID := seedImage(t, pool, blobDir, 3, 4)
	h := testImageHandler(pool, blobDir, panelMaxBytes)

	body := strings.NewReader(`{"image_id":` + strconv.FormatInt(imgID, 10) + `}`)
	if w := do(h, "PUT", "/api/devices/psn/image", "admin-tok", body, "application/json"); w.Code != http.StatusOK {
		t.Fatalf("PUT image = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if n := countManaged(t, pool, "psn"); n != 1 {
		t.Fatalf("managed playlists for psn = %d, want 1", n)
	}
	var plID, items int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM playlist WHERE managed_serial='psn'`).Scan(&plID); err != nil {
		t.Fatalf("read managed playlist: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM playlist_item WHERE playlist_id=$1 AND image_id=$2`, plID, imgID).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 1 {
		t.Fatalf("managed playlist holds %d matching items, want 1", items)
	}
	var boundPL int64
	if err := pool.QueryRow(context.Background(),
		`SELECT playlist_id FROM device_playlist_binding WHERE serial='psn'`).Scan(&boundPL); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if boundPL != plID {
		t.Fatalf("device bound to playlist %d, want the managed %d", boundPL, plID)
	}

	// Idempotent second PUT → still exactly one managed playlist (upsert, not growth).
	body2 := strings.NewReader(`{"image_id":` + strconv.FormatInt(imgID, 10) + `}`)
	if w := do(h, "PUT", "/api/devices/psn/image", "admin-tok", body2, "application/json"); w.Code != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200", w.Code)
	}
	if n := countManaged(t, pool, "psn"); n != 1 {
		t.Fatalf("after second PUT managed playlists = %d, want 1 (upsert, no growth)", n)
	}

	// Path a: device delete reaps the managed playlist.
	if found, err := devicestore.Delete(context.Background(), pool, "psn", false); err != nil || !found {
		t.Fatalf("device delete: found=%v err=%v", found, err)
	}
	if n := countManaged(t, pool, "psn"); n != 0 {
		t.Fatalf("managed playlist survived device delete = %d rows, want 0 (cleanup a)", n)
	}
}

// TestPanelImageDeleteManagedAware exercises cleanup (c): an image whose only reference is an "Aufs
// Panel" managed playlist is deletable (the managed playlist is unbound + reaped in the same tx),
// while an image referenced by an operator-authored playlist stays a 409.
func TestPanelImageDeleteManagedAware(t *testing.T) {
	pool := dbPool(t)
	blobDir := t.TempDir()
	seedOperator(t, pool, "admin-tok", true)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dsn','stable')`)
	h := testImageHandler(pool, blobDir, panelMaxBytes)

	// (c1) image only in a managed playlist → deletable.
	managedImg := seedImage(t, pool, blobDir, 5, 6)
	body := strings.NewReader(`{"image_id":` + strconv.FormatInt(managedImg, 10) + `}`)
	if w := do(h, "PUT", "/api/devices/dsn/image", "admin-tok", body, "application/json"); w.Code != http.StatusOK {
		t.Fatalf("PUT managed image = %d, want 200", w.Code)
	}
	if w := do(h, "DELETE", "/api/images/"+strconv.FormatInt(managedImg, 10), "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE managed-only image = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if n := countManaged(t, pool, "dsn"); n != 0 {
		t.Fatalf("managed playlist survived its image's delete = %d rows, want 0 (cleanup c)", n)
	}

	// (c2) image in an OPERATOR playlist → stays a 409 (never silently rehomed).
	realImg := seedImage(t, pool, blobDir, 7, 8)
	realPl, err := playliststore.CreatePlaylist(context.Background(), pool, playliststore.CreateParams{Name: "op-pl"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if _, err := playliststore.AddItem(context.Background(), pool, realPl.ID,
		playliststore.AddItemParams{ImageID: realImg}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if w := do(h, "DELETE", "/api/images/"+strconv.FormatInt(realImg, 10), "admin-tok", nil, ""); w.Code != http.StatusConflict {
		t.Fatalf("DELETE operator-referenced image = %d, want 409", w.Code)
	}
}
