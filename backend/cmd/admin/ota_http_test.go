package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/operator"
)

// --- DB property tests for the OTA admin routes (skipped unless TEST_DATABASE_URL is set) ---

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, stmt := range []string{
		`TRUNCATE rollout_targets`,
		`TRUNCATE command_queue`,
		`TRUNCATE telemetry`,
		`TRUNCATE logs`,
		`UPDATE channels SET default_version = NULL`,
		// image/playlist before operator_keys: image.operator_key_id -> operator_keys is RESTRICT, and
		// playlist_item.image_id -> image is RESTRICT (CASCADE clears the dependent rows in one shot).
		`TRUNCATE image, playlist, playlist_item RESTART IDENTITY CASCADE`,
		`DELETE FROM api_tokens`, // before operator_keys: created_by -> operator_keys is RESTRICT
		`DELETE FROM operator_keys`,
		`DELETE FROM faas_functions`, // FK CASCADE drops device_render_binding + faas_frame_lastgood
		// after faas_functions (its template_id -> templates is ON DELETE SET NULL; the referencing rows
		// are already gone, so the CASCADE has nothing to reach). A30 template CRUD isolation.
		`TRUNCATE templates RESTART IDENTITY CASCADE`,
		`DELETE FROM devices`,
		`DELETE FROM firmware_versions`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	adminhttp.ConfigureRateLimitsFromEnv() // fresh W4 rate-limit buckets per test (the post-auth principal
	// brake now lives inside adminhttp.Auth) — no cross-test carry-over into the gated handler suites.
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func seedOperator(t *testing.T, pool *pgxpool.Pool, token string, isAdmin bool) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO operator_keys (token_hash, label, is_admin) VALUES ($1,$2,$3)`,
		operator.HashToken(token), "test", isAdmin)
}

func testHandler(pool *pgxpool.Pool, blobDir string) http.Handler {
	mux := http.NewServeMux()
	registerOTARoutes(mux, pool, blobDir)
	return adminhttp.WithRequestID(mux)
}

func multipartFirmware(t *testing.T, version, sha string, blob []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("version", version)
	_ = mw.WriteField("sha256", sha)
	fw, err := mw.CreateFormFile("blob", "firmware.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(blob)
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func do(h http.Handler, method, path, bearer string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	// Operator-key requests reach admin over the SSH tunnel — the loopback listener. Tag the origin so
	// the W7 operator gate (loopback-only) honours the bearer, mirroring production BaseContext.
	r = r.WithContext(adminhttp.WithListenerOrigin(r.Context(), adminhttp.OriginLoopback))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// T9 — admin gating on the OTA routes (inherits Design 17's middleware, re-proven on this surface):
// a write route is 401 without a bearer, 403 for a non-admin key, and reachable for an admin; a read
// route is open to any valid key. Routes come from registerOTARoutes — the SAME wiring main.go mounts.
func TestOTAGating_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testHandler(pool, t.TempDir())

	if w := do(h, "POST", "/api/firmware", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("POST firmware no bearer = %d, want 401", w.Code)
	}
	if w := do(h, "POST", "/api/firmware", "ro-tok", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("POST firmware readonly = %d, want 403", w.Code)
	}
	// admin clears the gate (then a 400 for the absent multipart body — proves it is NOT 401/403)
	if w := do(h, "POST", "/api/firmware", "admin-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("POST firmware admin = %d, want past-the-gate", w.Code)
	}
	if w := do(h, "GET", "/api/firmware", "ro-tok", nil, ""); w.Code != http.StatusOK {
		t.Errorf("GET firmware readonly = %d, want 200", w.Code)
	}
}

// TestRegisterFirmwareHTTP_DB covers the register edge: T7 (malformed sha → 422 before the DB), the
// happy path (200 + row + blob on disk), the re-hash mismatch (422), and the duplicate (409).
func TestRegisterFirmwareHTTP_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	dir := t.TempDir()
	h := testHandler(pool, dir)
	blob := []byte("firmware-image-http")
	sha := sha256hex(blob)

	body, ct := multipartFirmware(t, "1.0.0", "NOT-HEX", blob)
	if w := do(h, "POST", "/api/firmware", "admin-tok", body, ct); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("T7 malformed sha = %d, want 422", w.Code)
	}

	body, ct = multipartFirmware(t, "1.0.0", sha, blob)
	if w := do(h, "POST", "/api/firmware", "admin-tok", body, ct); w.Code != http.StatusOK {
		t.Fatalf("register = %d (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(dir + "/" + sha + ".bin"); err != nil {
		t.Errorf("blob not written: %v", err)
	}

	body, ct = multipartFirmware(t, "2.0.0", sha256hex([]byte("other")), blob)
	if w := do(h, "POST", "/api/firmware", "admin-tok", body, ct); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("re-hash mismatch = %d, want 422", w.Code)
	}

	body, ct = multipartFirmware(t, "1.0.0", sha, blob)
	if w := do(h, "POST", "/api/firmware", "admin-tok", body, ct); w.Code != http.StatusConflict {
		t.Errorf("duplicate version = %d, want 409", w.Code)
	}
}

// TestResolveHTTP_DB — GET /api/resolve/{serial} returns the authoritative resolved target (T12: the
// same internal/rollout resolver ingest serves on /pp).
func TestResolveHTTP_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "ro-tok", false)
	mustExec(t, pool, `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ('v1',$1,'v1.bin',1)`, sha256hex([]byte("x")))
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)
	mustExec(t, pool, `UPDATE channels SET default_version='v1' WHERE name='stable'`)
	h := testHandler(pool, t.TempDir())

	w := do(h, "GET", "/api/resolve/dev1", "ro-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("resolve = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Success  bool `json:"success"`
		Resolved struct {
			Version string `json:"version"`
			Source  string `json:"source"`
		} `json:"resolved"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.Resolved.Version != "v1" || resp.Resolved.Source != "channel-default" {
		t.Errorf("resolved = %+v, want success v1/channel-default", resp.Resolved)
	}
}
