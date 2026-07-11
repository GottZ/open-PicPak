package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

func testOnboardDefaultsHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerOnboardDefaultsRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// TestOnboardDefaults_DB is the A35.3b red→green gate (probe a). The endpoint gates exactly like the fleet
// mutations (admin-only, RequireAdmin) and only an admin ever sees the real INGEST_TOKEN in the URLs:
//   - token set + admin  ⇒ 200 with the tokenized frame-/C2-URLs (design/26 §2.2 path scheme).
//   - non-admin (read-only key) ⇒ 403 (the secret never leaks to a read-only operator).
//   - no bearer ⇒ 401 (auth gate).
//   - token empty (feature dark) + admin ⇒ 200 with EMPTY URLs (SPA keeps its placeholder fallback).
func TestOnboardDefaults_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testOnboardDefaultsHandler(pool)
	const origin = "https://picpak.example"
	path := "/api/onboard/defaults?origin=" + origin

	// auth gate: no bearer ⇒ 401.
	if w := do(h, "GET", path, "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer = %d, want 401", w.Code)
	}
	// non-admin ⇒ 403, and the token never appears in the body.
	t.Setenv("INGEST_TOKEN", "S3CR3T")
	if w := do(h, "GET", path, "ro-tok", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("read-only key = %d, want 403", w.Code)
	} else if strings.Contains(w.Body.String(), "S3CR3T") {
		t.Errorf("403 body leaked the ingest token: %s", w.Body.String())
	}

	// token set + admin ⇒ 200 with the tokenized URLs.
	w := do(h, "GET", path, "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin (token set) = %d (%s)", w.Code, w.Body.String())
	}
	body := jsonBody(t, w)
	if got := body["frame_url"]; got != origin+"/S3CR3T/frame" {
		t.Errorf("frame_url = %q, want %q", got, origin+"/S3CR3T/frame")
	}
	if got := body["c2_url"]; got != origin+"/S3CR3T/c2" {
		t.Errorf("c2_url = %q, want %q", got, origin+"/S3CR3T/c2")
	}
	if got := body["c2_period_s"]; got != float64(1800) {
		t.Errorf("c2_period_s = %v, want 1800", got)
	}

	// token empty (feature dark) + admin ⇒ 200 with EMPTY URLs.
	t.Setenv("INGEST_TOKEN", "")
	w = do(h, "GET", path, "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("admin (token empty) = %d (%s)", w.Code, w.Body.String())
	}
	body = jsonBody(t, w)
	if body["frame_url"] != "" || body["c2_url"] != "" {
		t.Errorf("token empty must yield blank URLs, got frame=%q c2=%q", body["frame_url"], body["c2_url"])
	}
}
