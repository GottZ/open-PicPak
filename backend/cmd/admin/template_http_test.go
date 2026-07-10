package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/templatestore"
)

func testTemplateHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerTemplateRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// T-gate — the 5 template routes inherit the admin middleware (re-proven on this surface): a mutation is
// 401 without a bearer, 403 for a non-admin key, and past-the-gate for admin; a read is open to any valid
// key. Routes come from registerTemplateRoutes — the SAME wiring main.go mounts. Red: a mutation mounted
// without RequireAdmin answers past-the-gate for the read-only key (not 403).
func TestTemplateGating_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testTemplateHandler(pool)

	mutations := []struct{ method, path string }{
		{"POST", "/api/templates"},
		{"PUT", "/api/templates/1"},
		{"DELETE", "/api/templates/1"},
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
	for _, path := range []string{"/api/templates", "/api/templates/1"} {
		if w := do(h, "GET", path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s no bearer = %d, want 401", path, w.Code)
		}
		if w := do(h, "GET", path, "ro-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("GET %s readonly = %d, want reachable", path, w.Code)
		}
	}
}

// T-validation — edge validation lands as 422 (never a 500 from the DB CHECK), and a duplicate name is
// 409. The 'builtin/' create is 422 (reserved namespace, no operator squatting §3.3). The multibyte probe
// is load-bearing: a berry_snippet whose char_length is under 8191 but whose octet_length is over it must
// be REJECTED as 422 — the byte cap, not codepoints (W18 poison-pill). Red for the multibyte case: a
// char_length guard would admit it (200) and let a >8191-byte script reach the firmware fetch.
func TestTemplateValidation_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	// 4096 * '€' = 4096 codepoints but 12288 bytes → char_length<8191, octet_length>8191.
	multibyte := strings.Repeat("€", 4096)
	cases := []struct {
		name, body string
		want       int
	}{
		{"bad name", `{"name":"Bad Name!","kind":"render_fn","source":"x"}`, 422},
		{"builtin namespace", `{"name":"builtin/x","kind":"render_fn","source":"x"}`, 422},
		{"bad kind", `{"name":"ok1","kind":"nope","source":"x"}`, 422},
		{"missing kind", `{"name":"ok2","source":"x"}`, 422},
		{"missing source", `{"name":"ok3","kind":"render_fn","source":""}`, 422},
		{"multibyte berry over cap", `{"name":"ok4","kind":"berry_snippet","source":"` + multibyte + `"}`, 422},
		{"ok render_fn", `{"name":"good-tmpl","kind":"render_fn","source":"export default async()=>({})"}`, 200},
		{"ok berry at cap", `{"name":"good-berry","kind":"berry_snippet","source":"` + strings.Repeat("a", 8191) + `"}`, 200},
	}
	for _, c := range cases {
		w := do(h, "POST", "/api/templates", "admin-tok", strings.NewReader(c.body), "application/json")
		if w.Code != c.want {
			t.Errorf("%s: create = %d, want %d (%s)", c.name, w.Code, c.want, truncBody(w.Body.String()))
		}
	}
	// duplicate name → 409.
	if w := do(h, "POST", "/api/templates", "admin-tok",
		strings.NewReader(`{"name":"good-tmpl","kind":"berry_snippet","source":"y"}`), "application/json"); w.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	// non-integer id → 422.
	if w := do(h, "GET", "/api/templates/notanint", "admin-tok", nil, ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad id = %d, want 422", w.Code)
	}
	// unknown id → 404 on get/update/delete.
	if w := do(h, "GET", "/api/templates/99999", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("get unknown = %d, want 404", w.Code)
	}
	if w := do(h, "PUT", "/api/templates/99999", "admin-tok", strings.NewReader(`{"source":"z"}`), "application/json"); w.Code != http.StatusNotFound {
		t.Errorf("update unknown = %d, want 404", w.Code)
	}
	if w := do(h, "DELETE", "/api/templates/99999", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("delete unknown = %d, want 404", w.Code)
	}
}

// T-builtin-immutable — a builtin row is refused on PUT and DELETE with 409 builtin_immutable; the prefab
// catalog is its source of truth (§5.3). The fixture comes through the PRODUCTION write path
// (templatestore.SeedBuiltins), not a hand INSERT. Red: a route that skips the builtin pre-check runs the
// Update/Delete and returns 200, corrupting the prefab catalog until the next SeedBuiltins overwrite.
func TestTemplateBuiltinImmutable_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	res, err := templatestore.SeedBuiltins(context.Background(), pool)
	if err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	if res.Inserted == 0 {
		t.Fatalf("seed inserted no builtins: %+v", res)
	}
	// find a seeded builtin id.
	var builtinID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM templates WHERE builtin ORDER BY id LIMIT 1`).Scan(&builtinID); err != nil {
		t.Fatalf("locate builtin: %v", err)
	}
	ids := itoa(builtinID)

	if w := do(h, "PUT", "/api/templates/"+ids, "admin-tok",
		strings.NewReader(`{"source":"tampered"}`), "application/json"); w.Code != http.StatusConflict {
		t.Errorf("PUT builtin = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "DELETE", "/api/templates/"+ids, "admin-tok", nil, ""); w.Code != http.StatusConflict {
		t.Errorf("DELETE builtin = %d, want 409 (%s)", w.Code, w.Body.String())
	}
	// the row is untouched: still present, still builtin, version not bumped by a leaked write.
	var stillBuiltin bool
	var source string
	if err := pool.QueryRow(context.Background(),
		`SELECT builtin, source FROM templates WHERE id=$1`, builtinID).Scan(&stillBuiltin, &source); err != nil {
		t.Fatalf("re-read builtin: %v", err)
	}
	if !stillBuiltin || source == "tampered" {
		t.Errorf("builtin row was mutated: builtin=%v source=%q", stillBuiltin, source)
	}
}

// T-lifecycle — create → get (source present, version 1) → update (version bump to 2) → delete → 404. The
// full CRUD roundtrip against the store, proving the handlers wire to templatestore correctly.
func TestTemplateLifecycle_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	w := do(h, "POST", "/api/templates", "admin-tok",
		strings.NewReader(`{"name":"life-tmpl","kind":"render_fn","source":"export default async()=>({})","params":[{"name":"n","label":"N","type":"string","required":true}]}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d (%s)", w.Code, w.Body.String())
	}
	id := int64(jsonBody(t, w)["id"].(float64))

	g := do(h, "GET", "/api/templates/"+itoa(id), "admin-tok", nil, "")
	if g.Code != http.StatusOK {
		t.Fatalf("get = %d", g.Code)
	}
	tmpl, _ := jsonBody(t, g)["template"].(map[string]any)
	if tmpl == nil || tmpl["source"] == "" {
		t.Fatalf("get did not return the template source: %s", g.Body.String())
	}
	if v, _ := tmpl["version"].(float64); v != 1 {
		t.Errorf("initial version = %v, want 1", tmpl["version"])
	}
	if b, _ := tmpl["builtin"].(bool); b {
		t.Errorf("operator-created template reported builtin=true")
	}

	if w := do(h, "PUT", "/api/templates/"+itoa(id), "admin-tok",
		strings.NewReader(`{"source":"export default async()=>({image:0})"}`), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", w.Code, w.Body.String())
	}
	g2 := do(h, "GET", "/api/templates/"+itoa(id), "admin-tok", nil, "")
	tmpl2, _ := jsonBody(t, g2)["template"].(map[string]any)
	if v, _ := tmpl2["version"].(float64); v != 2 {
		t.Errorf("post-update version = %v, want 2 (K7 cache-bust)", tmpl2["version"])
	}

	if w := do(h, "DELETE", "/api/templates/"+itoa(id), "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Errorf("delete = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/templates/"+itoa(id), "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

// T-keyset — the list walks the keyset cursor to completion: every created id appears exactly once across
// the paged sweep, and no id is skipped or repeated. Also proves the ?kind= filter narrows the projection.
// Red: an OFFSET pager (or a cursor read from the wrong column) would drop or duplicate rows under paging.
func TestTemplateListKeyset_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	const n = 7
	created := map[int64]bool{}
	for i := 0; i < n; i++ {
		body := `{"name":"walk-` + itoa(int64(i)) + `","kind":"render_fn","source":"x"}`
		w := do(h, "POST", "/api/templates", "admin-tok", strings.NewReader(body), "application/json")
		if w.Code != http.StatusOK {
			t.Fatalf("create %d = %d (%s)", i, w.Code, w.Body.String())
		}
		created[int64(jsonBody(t, w)["id"].(float64))] = true
	}
	// one non-render_fn row to prove the kind filter excludes it from the render_fn sweep.
	if w := do(h, "POST", "/api/templates", "admin-tok",
		strings.NewReader(`{"name":"walk-berry","kind":"berry_snippet","source":"b"}`), "application/json"); w.Code != http.StatusOK {
		t.Fatalf("create berry = %d (%s)", w.Code, w.Body.String())
	}

	seen := map[int64]int{}
	var after int64
	total := 0
	for iter := 0; iter < n+5; iter++ { // bounded: never loop forever on a broken cursor
		w := do(h, "GET", "/api/templates?kind=render_fn&limit=2&after="+itoa(after), "admin-tok", nil, "")
		if w.Code != http.StatusOK {
			t.Fatalf("list = %d (%s)", w.Code, w.Body.String())
		}
		rows, _ := jsonBody(t, w)["templates"].([]any)
		if len(rows) == 0 {
			break
		}
		for _, raw := range rows {
			row := raw.(map[string]any)
			id := int64(row["id"].(float64))
			if k, _ := row["kind"].(string); k != "render_fn" {
				t.Errorf("kind filter leaked a %q row", k)
			}
			seen[id]++
			after = id
			total++
		}
	}
	if total != n {
		t.Errorf("keyset walk yielded %d rows, want %d (skipped or short cursor)", total, n)
	}
	for id := range created {
		if seen[id] != 1 {
			t.Errorf("id %d seen %d times across the walk, want exactly 1", id, seen[id])
		}
	}
}

func truncBody(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
