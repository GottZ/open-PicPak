package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

func testFaasHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerFaasRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return m
}

// T-gate — the 7 FaaS routes inherit Design 17's middleware (re-proven on this surface): a mutation is
// 401 without a bearer, 403 for a non-admin key, and past-the-gate for admin; a read is open to any valid
// key. Routes come from registerFaasRoutes — the SAME wiring main.go mounts.
func TestFaasGating_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testFaasHandler(pool)

	mutations := []struct{ method, path string }{
		{"POST", "/api/functions"},
		{"PUT", "/api/functions/1"},
		{"PATCH", "/api/functions/1"},
		{"DELETE", "/api/functions/1"},
		{"PUT", "/api/devices/testsn/render"},
	}
	for _, m := range mutations {
		if w := do(h, m.method, m.path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no bearer = %d, want 401", m.method, m.path, w.Code)
		}
		if w := do(h, m.method, m.path, "ro-tok", nil, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s %s readonly = %d, want 403", m.method, m.path, w.Code)
		}
		if w := do(h, m.method, m.path, "admin-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("%s %s admin = %d, want past-the-gate", m.method, m.path, w.Code)
		}
	}
	// reads: open to any valid key (401 without a bearer, but a read-only key clears — not 403).
	for _, path := range []string{"/api/functions", "/api/functions/1"} {
		if w := do(h, "GET", path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s no bearer = %d, want 401", path, w.Code)
		}
		if w := do(h, "GET", path, "ro-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("GET %s readonly = %d, want reachable", path, w.Code)
		}
	}
}

// T-webhook-once — a webhook function's token is generated + returned ONCE at create, its sha256 stored,
// and NEVER echoed by GET/list (K8). Red: the plaintext lands in the DB, or a metadata read leaks it.
func TestFaasWebhookTokenOnce_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testFaasHandler(pool)
	ctx := context.Background()

	w := do(h, "POST", "/api/functions", "admin-tok",
		strings.NewReader(`{"name":"hook1","source":"x","trigger_type":"webhook"}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("create webhook = %d (%s)", w.Code, w.Body.String())
	}
	body := jsonBody(t, w)
	tok, _ := body["webhook_token"].(string)
	if tok == "" {
		t.Fatal("create webhook did not return a plaintext token")
	}
	idF, _ := body["id"].(float64)
	id := int64(idF)

	// stored column is sha256(token), never the plaintext.
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT webhook_token_sha256 FROM faas_functions WHERE id=$1`, id).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	want := sha256.Sum256([]byte(tok))
	if hex.EncodeToString(stored) != hex.EncodeToString(want[:]) {
		t.Errorf("stored hash != sha256(token) — plaintext or wrong digest at rest")
	}

	// GET {id} and list must NOT echo the token (no "webhook_token" key anywhere in the read shapes).
	g := do(h, "GET", "/api/functions/"+itoa(id), "admin-tok", nil, "")
	if strings.Contains(g.Body.String(), "webhook_token") || strings.Contains(g.Body.String(), tok) {
		t.Errorf("GET function leaked the token: %s", g.Body.String())
	}
	l := do(h, "GET", "/api/functions", "admin-tok", nil, "")
	if strings.Contains(l.Body.String(), tok) {
		t.Errorf("list leaked the token: %s", l.Body.String())
	}

	// rotate via PATCH → a NEW plaintext, shown once; the stored hash changes.
	r := do(h, "PATCH", "/api/functions/"+itoa(id), "admin-tok", strings.NewReader(`{"rotate_token":true}`), "application/json")
	if r.Code != http.StatusOK {
		t.Fatalf("rotate = %d (%s)", r.Code, r.Body.String())
	}
	tok2, _ := jsonBody(t, r)["webhook_token"].(string)
	if tok2 == "" || tok2 == tok {
		t.Errorf("rotate did not mint a fresh token (got %q, was %q)", tok2, tok)
	}
}

// T-validation — edge validation lands as 422 (not a 500 from the DB), and a duplicate name is 409.
func TestFaasValidation_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testFaasHandler(pool)

	cases := []struct {
		name, body string
		want       int
	}{
		{"bad name", `{"name":"Bad Name!","source":"x"}`, 422},
		{"missing source", `{"name":"ok","source":""}`, 422},
		{"bad trigger", `{"name":"ok","source":"x","trigger_type":"nope"}`, 422},
		{"bad config shape", `{"name":"ok","source":"x","trigger_config":[1,2]}`, 422},
		{"bad render mode", `{"name":"ok","source":"x","trigger_config":{"mode":"weird"}}`, 422},
		{"neg ttl", `{"name":"ok","source":"x","trigger_config":{"ttl_s":-1}}`, 422},
		{"ok create", `{"name":"good-fn","source":"x"}`, 200},
	}
	for _, c := range cases {
		w := do(h, "POST", "/api/functions", "admin-tok", strings.NewReader(c.body), "application/json")
		if w.Code != c.want {
			t.Errorf("%s: create = %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}
	// duplicate name → 409.
	if w := do(h, "POST", "/api/functions", "admin-tok", strings.NewReader(`{"name":"good-fn","source":"y"}`), "application/json"); w.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	// non-integer id → 422.
	if w := do(h, "GET", "/api/functions/notanint", "admin-tok", nil, ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad id = %d, want 422", w.Code)
	}
	// bind an unknown serial → 422 (device_render_binding has no FK, edge-checked).
	if w := do(h, "PUT", "/api/devices/ghost/render", "admin-tok", strings.NewReader(`{"function_id":1}`), "application/json"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bind unknown serial = %d, want 422 (%s)", w.Code, w.Body.String())
	}
}

// T-lifecycle — create → get (source present) → patch enabled → bind a real device → update (version bump)
// → delete. The full happy path against the store, proving the handlers wire to faasstore correctly.
func TestFaasLifecycle_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testFaasHandler(pool)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('life-sn','stable')`)

	w := do(h, "POST", "/api/functions", "admin-tok",
		strings.NewReader(`{"name":"life-fn","source":"export default async()=>({})","trigger_type":"render","trigger_config":{"mode":"sync","ttl_s":60}}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	id := int64(jsonBody(t, w)["id"].(float64))

	g := do(h, "GET", "/api/functions/"+itoa(id), "admin-tok", nil, "")
	if g.Code != http.StatusOK {
		t.Fatalf("get = %d", g.Code)
	}
	fn, _ := jsonBody(t, g)["function"].(map[string]any)
	if fn == nil || fn["source"] == "" {
		t.Errorf("get did not return the function source: %s", g.Body.String())
	}
	if v, _ := fn["version"].(float64); v != 1 {
		t.Errorf("initial version = %v, want 1", fn["version"])
	}

	if w := do(h, "PATCH", "/api/functions/"+itoa(id), "admin-tok", strings.NewReader(`{"enabled":true}`), "application/json"); w.Code != http.StatusOK {
		t.Errorf("patch enabled = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "PUT", "/api/devices/life-sn/render", "admin-tok", strings.NewReader(`{"function_id":`+itoa(id)+`}`), "application/json"); w.Code != http.StatusOK {
		t.Errorf("bind = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "PUT", "/api/functions/"+itoa(id), "admin-tok", strings.NewReader(`{"source":"export default async()=>({image:0})"}`), "application/json"); w.Code != http.StatusOK {
		t.Errorf("update = %d (%s)", w.Code, w.Body.String())
	}
	// version bumped by the update (K7 cache-bust).
	g2 := do(h, "GET", "/api/functions/"+itoa(id), "admin-tok", nil, "")
	fn2, _ := jsonBody(t, g2)["function"].(map[string]any)
	if v, _ := fn2["version"].(float64); v != 2 {
		t.Errorf("post-update version = %v, want 2", fn2["version"])
	}
	// bound serial shows up on the function.
	bs, _ := jsonBody(t, g2)["bound_serials"].([]any)
	if len(bs) != 1 || bs[0] != "life-sn" {
		t.Errorf("bound_serials = %v, want [life-sn]", bs)
	}

	if w := do(h, "DELETE", "/api/functions/"+itoa(id), "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Errorf("delete = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/functions/"+itoa(id), "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
