package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/apitoken"
	"github.com/open-picpak/backend/internal/imgstore"
)

// --- DB property tests for the A28 W5 image CRUD routes (skipped unless TEST_DATABASE_URL is set) ---

func testImageHandler(pool *pgxpool.Pool, blobDir string, maxBytes int64) http.Handler {
	mux := http.NewServeMux()
	registerImageRoutes(mux, pool, blobDir, maxBytes)
	return adminhttp.WithRequestID(mux)
}

// pngBytes encodes a real PNG via the stdlib encoder (the production decode path re-reads it) — a
// hand-built byte fixture would risk sharing the sniffer's blind spots (W10). Distinct (w,h) yield
// distinct content, hence distinct sha256 for the pagination/dedup fixtures.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x*13 + y*7), uint8(x * 3), uint8(y * 5), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func multipartImage(t *testing.T, blob []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "img.png")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(blob); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func mintScoped(t *testing.T, pool *pgxpool.Pool, label string, scopes []string) string {
	t.Helper()
	tok, _, err := apitoken.Create(context.Background(), pool, label, scopes, nil, nil)
	if err != nil {
		t.Fatalf("mint %s token: %v", label, err)
	}
	return tok
}

// TestImageGating_DB — the W5 probes: unauth → 401, an image:read-only token on POST → 403, an
// image:write token past the gate (201 create), and the mirror on reads (an image:write-only token is
// 403 on the read route, an image:read token is 200). Red (route without RequireScope): the read-only
// token reaches the upload handler instead of a 403. Routes come from registerImageRoutes — the SAME
// wiring main.go mounts.
func TestImageGating_DB(t *testing.T) {
	pool := dbPool(t)
	readTok := mintScoped(t, pool, "ro", []string{adminhttp.ScopeImageRead})
	writeTok := mintScoped(t, pool, "wo", []string{adminhttp.ScopeImageWrite})
	h := testImageHandler(pool, t.TempDir(), defaultMaxImageBytes)
	png := pngBytes(t, 4, 4)

	// POST /api/images (image:write gate)
	if w := do(h, "POST", "/api/images", "", bytes.NewReader(png), "image/png"); w.Code != http.StatusUnauthorized {
		t.Errorf("POST no credential = %d, want 401", w.Code)
	}
	if w := do(h, "POST", "/api/images", readTok, bytes.NewReader(png), "image/png"); w.Code != http.StatusForbidden {
		t.Errorf("POST image:read-only = %d, want 403 (scope gate)", w.Code)
	}
	if w := do(h, "POST", "/api/images", writeTok, bytes.NewReader(png), "image/png"); w.Code != http.StatusCreated {
		t.Errorf("POST image:write = %d, want 201 (past the gate)", w.Code)
	}

	// GET /api/images (image:read gate)
	if w := do(h, "GET", "/api/images", readTok, nil, ""); w.Code != http.StatusOK {
		t.Errorf("GET list image:read = %d, want 200", w.Code)
	}
	if w := do(h, "GET", "/api/images", writeTok, nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("GET list image:write-only = %d, want 403 (lacks read scope)", w.Code)
	}
}

// TestImageMimeAllowlist_DB — the B5 layer-(a) probe: an SVG and a plain-text body are both 422
// (unsupported_type) while a real PNG is admitted. Red (naive route without the DetectContentType
// allowlist): the SVG/text upload is stored and later served same-origin as an XSS vector (§5 B5).
func TestImageMimeAllowlist_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageWrite})
	h := testImageHandler(pool, t.TempDir(), defaultMaxImageBytes)

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><script>alert(1)</script></svg>`)
	if w := do(h, "POST", "/api/images", writeTok, bytes.NewReader(svg), "image/svg+xml"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST svg = %d, want 422 (not in allowlist)", w.Code)
	}
	txt := []byte("this is plain text, most certainly not a raster image of any kind at all")
	if w := do(h, "POST", "/api/images", writeTok, bytes.NewReader(txt), "text/plain"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("POST text/plain = %d, want 422", w.Code)
	}
	if w := do(h, "POST", "/api/images", writeTok, bytes.NewReader(pngBytes(t, 4, 4)), "image/png"); w.Code != http.StatusCreated {
		t.Errorf("POST png = %d, want 201", w.Code)
	}
}

// TestImageIdempotent_DB — the content-addressed idempotency probe: a first POST creates (201), a
// re-POST of identical bytes is a dedup hit (200) carrying the SAME id, and exactly ONE row exists.
// Red (no sha256-UNIQUE dedup): the re-POST inserts a duplicate row/blob and returns a new id.
func TestImageIdempotent_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageWrite})
	h := testImageHandler(pool, t.TempDir(), defaultMaxImageBytes)
	png := pngBytes(t, 5, 5)

	w1 := do(h, "POST", "/api/images", writeTok, bytes.NewReader(png), "image/png")
	if w1.Code != http.StatusCreated {
		t.Fatalf("first POST = %d, want 201 (%s)", w1.Code, w1.Body.String())
	}
	id1 := jsonBody(t, w1)["id"]

	w2 := do(h, "POST", "/api/images", writeTok, bytes.NewReader(png), "image/png")
	if w2.Code != http.StatusOK {
		t.Fatalf("re-POST = %d, want 200 (dedup hit) (%s)", w2.Code, w2.Body.String())
	}
	id2 := jsonBody(t, w2)["id"]

	if id1 != id2 {
		t.Fatalf("dedup id mismatch: first=%v re-post=%v", id1, id2)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM image`).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("image rows after re-POST = %d, want 1 (no duplicate)", n)
	}
}

// TestImageOversized_DB — the byte-cap probe: a blob one byte over the env cap is 422 (blob_too_large),
// while the SAME blob passes when the cap is at its size and it is sent multipart (the +1<<20 form
// overhead grace is absorbed, ota_http.go:54 parity). Red (no cap / cap without form overhead): the
// oversized blob is stored / a max-sized multipart upload is wrongly rejected.
func TestImageOversized_DB(t *testing.T) {
	pool := dbPool(t)
	writeTok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageWrite})
	png := pngBytes(t, 24, 24)

	// Cap one byte below the blob → 422.
	hSmall := testImageHandler(pool, t.TempDir(), int64(len(png)-1))
	if w := do(hSmall, "POST", "/api/images", writeTok, bytes.NewReader(png), "image/png"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("oversized raw POST = %d, want 422", w.Code)
	}

	// Cap exactly at the blob size, sent multipart: the form envelope pushes the request past the cap,
	// but the +imageFormOverhead grace admits it and the LimitReader keeps the blob itself within cap.
	hExact := testImageHandler(pool, t.TempDir(), int64(len(png)))
	buf, ct := multipartImage(t, png)
	if w := do(hExact, "POST", "/api/images", writeTok, buf, ct); w.Code != http.StatusCreated {
		t.Errorf("max-sized multipart POST = %d, want 201 (form overhead grace) (%s)", w.Code, buf.String())
	}
}

// TestImageRawServe_Headers_DB — the B5 layers-(b)+(c) probe: ?raw=1 serves the stored, upload-validated
// mime as Content-Type (never re-sniffed) plus nosniff, the tightest CSP, and inline disposition; the
// bytes round-trip exactly. Red (headers absent / Content-Type re-guessed): a polyglot serves under a
// script-executable type same-origin.
func TestImageRawServe_Headers_DB(t *testing.T) {
	pool := dbPool(t)
	tok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageRead, adminhttp.ScopeImageWrite})
	h := testImageHandler(pool, t.TempDir(), defaultMaxImageBytes)
	png := pngBytes(t, 6, 6)

	pw := do(h, "POST", "/api/images", tok, bytes.NewReader(png), "image/png")
	if pw.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", pw.Code, pw.Body.String())
	}
	id := int64(jsonBody(t, pw)["id"].(float64))

	rw := do(h, "GET", "/api/images/"+strconv.FormatInt(id, 10)+"?raw=1", tok, nil, "")
	if rw.Code != http.StatusOK {
		t.Fatalf("raw serve = %d (%s)", rw.Code, rw.Body.String())
	}
	if got := rw.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png (stored, upload-validated mime)", got)
	}
	if got := rw.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rw.Header().Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Errorf("Content-Security-Policy = %q, want default-src 'none'", got)
	}
	if got := rw.Header().Get("Content-Disposition"); got == "" || got[:6] != "inline" {
		t.Errorf("Content-Disposition = %q, want inline; filename=...", got)
	}
	if !bytes.Equal(rw.Body.Bytes(), png) {
		t.Errorf("raw body != uploaded bytes (%d vs %d)", rw.Body.Len(), len(png))
	}
}

// countingTracer counts every query issued on a pool (pgx.QueryTracer) — the external signal for the
// N+1 probe (W19: a measured query count, not a code-reading assertion).
type countingTracer struct{ n atomic.Int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}
func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestImageList_Pagination_DB — the keyset probe: a limit=2 walk over 5 images covers every row exactly
// once and ends with a null cursor. Plus the N+1 probe: listing all rows issues EXACTLY ONE query
// regardless of row count (a per-row playlist fanout would be 1+N). Red (OFFSET drift / per-row fanout):
// a row is missed or double-counted / the query count scales with N.
func TestImageList_Pagination_DB(t *testing.T) {
	pool := dbPool(t)
	readTok := mintScoped(t, pool, "ro", []string{adminhttp.ScopeImageRead})
	blobDir := t.TempDir()
	h := testImageHandler(pool, blobDir, defaultMaxImageBytes)
	ctx := context.Background()

	// Seed 5 distinct images through the production store write path (PutImage).
	want := map[int64]bool{}
	for i := 0; i < 5; i++ {
		img, err := imgstore.PutImage(ctx, pool, blobDir, nil, pngBytes(t, i+2, i+2))
		if err != nil {
			t.Fatalf("seed image %d: %v", i, err)
		}
		want[img.ID] = true
	}

	seen := map[int64]int{}
	cursor := ""
	pages := 0
	for {
		path := "/api/images?limit=2"
		if cursor != "" {
			path += "&after=" + cursor
		}
		w := do(h, "GET", path, readTok, nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list page = %d (%s)", w.Code, w.Body.String())
		}
		body := jsonBody(t, w)
		imgs, _ := body["images"].([]any)
		for _, raw := range imgs {
			m := raw.(map[string]any)
			seen[int64(m["id"].(float64))]++
		}
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if body["next_cursor"] == nil {
			break
		}
		cursor = strconv.FormatInt(int64(body["next_cursor"].(float64)), 10)
	}

	if len(seen) != len(want) {
		t.Fatalf("covered %d distinct ids, want %d", len(seen), len(want))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Fatalf("image id %d seen %d times, want exactly 1", id, seen[id])
		}
	}

	// N+1 probe: the store issues ONE query for the whole page, independent of row count.
	var counter countingTracer
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = &counter
	tp, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(tp.Close)
	if _, err := tp.Exec(ctx, "SELECT 1"); err != nil { // warm the conn so its setup queries are not counted
		t.Fatalf("warm: %v", err)
	}
	counter.n.Store(0)
	if _, err := imgstore.ListImages(ctx, tp, 100, 0); err != nil {
		t.Fatalf("traced list: %v", err)
	}
	if got := counter.n.Load(); got != 1 {
		t.Fatalf("ListImages over %d rows issued %d queries, want exactly 1 (no per-row fanout)", len(want), got)
	}
}

// TestImageDelete_DB — the delete probe: a delete removes the row AND the blob file, writes exactly one
// synchronous image.delete audit row (target=id), and an unknown id is a uniform 404. Red (no blob GC /
// no audit): the blob orphans on disk / the trail is silent.
func TestImageDelete_DB(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool) // clean admin_audit for the exact-count assertion
	tok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageRead, adminhttp.ScopeImageWrite})
	blobDir := t.TempDir()
	h := testImageHandler(pool, blobDir, defaultMaxImageBytes)

	pw := do(h, "POST", "/api/images", tok, bytes.NewReader(pngBytes(t, 7, 7)), "image/png")
	if pw.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", pw.Code, pw.Body.String())
	}
	body := jsonBody(t, pw)
	id := int64(body["id"].(float64))
	sha := body["sha256"].(string)
	blobFile := filepath.Join(blobDir, sha+".bin")
	if _, err := os.Stat(blobFile); err != nil {
		t.Fatalf("blob not written pre-delete: %v", err)
	}

	if w := do(h, "DELETE", "/api/images/"+strconv.FormatInt(id, 10), tok, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/images/"+strconv.FormatInt(id, 10), tok, nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404 (row gone)", w.Code)
	}
	if _, err := os.Stat(blobFile); !os.IsNotExist(err) {
		t.Errorf("blob still on disk after delete: err=%v", err)
	}
	if n := sessionAuditCount(t, pool, "image.delete", strconv.FormatInt(id, 10)); n != 1 {
		t.Errorf("image.delete audit rows (target=%d) = %d, want 1", id, n)
	}
	if w := do(h, "DELETE", "/api/images/999999", tok, nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown id = %d, want 404", w.Code)
	}
}

// TestImageDelete_InUse_DB — the referential-integrity probe: an image referenced by a playlist_item is
// a 409 (image_in_use), not a delete that would strand the playlist. Red (no FK RESTRICT surfaced): the
// referenced image is deleted.
func TestImageDelete_InUse_DB(t *testing.T) {
	pool := dbPool(t)
	tok := mintScoped(t, pool, "rw", []string{adminhttp.ScopeImageWrite})
	blobDir := t.TempDir()
	h := testImageHandler(pool, blobDir, defaultMaxImageBytes)
	ctx := context.Background()

	pw := do(h, "POST", "/api/images", tok, bytes.NewReader(pngBytes(t, 8, 8)), "image/png")
	if pw.Code != http.StatusCreated {
		t.Fatalf("POST = %d (%s)", pw.Code, pw.Body.String())
	}
	id := int64(jsonBody(t, pw)["id"].(float64))

	var plID int64
	if err := pool.QueryRow(ctx, `INSERT INTO playlist (name) VALUES ('gate-inuse') RETURNING id`).Scan(&plID); err != nil {
		t.Fatalf("insert playlist: %v", err)
	}
	mustExec(t, pool, `INSERT INTO playlist_item (playlist_id, image_id, position) VALUES ($1, $2, 1)`, plID, id)

	if w := do(h, "DELETE", "/api/images/"+strconv.FormatInt(id, 10), tok, nil, ""); w.Code != http.StatusConflict {
		t.Fatalf("DELETE in-use image = %d, want 409 (image_in_use)", w.Code)
	}
}
