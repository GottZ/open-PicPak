package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// testFaasUIHandler mounts BOTH the A24 store CRUD (create + bind, used to set up state) and the A25
// editor-support routes under test — the SAME wiring main.go mounts. The GET/DELETE on
// /api/devices/{serial}/render coexist with A24's PUT (distinct method patterns).
func testFaasUIHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerFaasRoutes(mux, pool)
	registerFaasUIRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// T-gate (A25) — the binding READS are open to any valid key (401 without a bearer, reachable for a
// read-only key); the unbind is a mutation → 401/403/past-the-gate. Re-proves Design 17's middleware on
// the exact registerFaasUIRoutes wiring.
func TestFaasUIGating_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testFaasUIHandler(pool)

	// unbind is admin-only.
	unbind := struct{ method, path string }{"DELETE", "/api/devices/testsn/render"}
	if w := do(h, unbind.method, unbind.path, "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("%s %s no bearer = %d, want 401", unbind.method, unbind.path, w.Code)
	}
	if w := do(h, unbind.method, unbind.path, "ro-tok", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("%s %s readonly = %d, want 403", unbind.method, unbind.path, w.Code)
	}
	if w := do(h, unbind.method, unbind.path, "admin-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("%s %s admin = %d, want past-the-gate", unbind.method, unbind.path, w.Code)
	}

	// reads: 401 without a bearer, but a read-only key clears (not 403).
	for _, path := range []string{"/api/devices/testsn/render", "/api/functions/1/devices"} {
		if w := do(h, "GET", path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s no bearer = %d, want 401", path, w.Code)
		}
		if w := do(h, "GET", path, "ro-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("GET %s readonly = %d, want reachable", path, w.Code)
		}
	}
}

// T12 — the binding reads + unbind are correct: the reverse read counts the blast radius exactly, the
// forward read names the bound function, an unbound device reads null, an unknown function is 404, and the
// unbind drops the row (the device falls back to "no function"). Red: a miscount under-warns a fleet-wide
// source edit; a stale forward read mislabels the binding.
func TestFaasBindingReads_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testFaasUIHandler(pool)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('bind-a','stable'), ('bind-b','stable'), ('bind-c','stable')`)

	// create a function and bind two of the three devices to it (via the A24 write routes).
	cw := do(h, "POST", "/api/functions", "admin-tok",
		strings.NewReader(`{"name":"blast-fn","source":"export default async()=>({})"}`), "application/json")
	if cw.Code != http.StatusOK {
		t.Fatalf("create = %d (%s)", cw.Code, cw.Body.String())
	}
	id := int64(jsonBody(t, cw)["id"].(float64))
	for _, s := range []string{"bind-a", "bind-b"} {
		if w := do(h, "PUT", "/api/devices/"+s+"/render", "admin-tok",
			strings.NewReader(`{"function_id":`+itoa(id)+`}`), "application/json"); w.Code != http.StatusOK {
			t.Fatalf("bind %s = %d (%s)", s, w.Code, w.Body.String())
		}
	}

	// reverse read (blast radius) = exactly {bind-a, bind-b}, count 2.
	rw := do(h, "GET", "/api/functions/"+itoa(id)+"/devices", "admin-tok", nil, "")
	if rw.Code != http.StatusOK {
		t.Fatalf("reverse read = %d (%s)", rw.Code, rw.Body.String())
	}
	rb := jsonBody(t, rw)
	if c, _ := rb["count"].(float64); c != 2 {
		t.Errorf("blast-radius count = %v, want 2", rb["count"])
	}
	serials, _ := rb["serials"].([]any)
	if len(serials) != 2 || serials[0] != "bind-a" || serials[1] != "bind-b" {
		t.Errorf("blast-radius serials = %v, want [bind-a bind-b]", serials)
	}

	// forward read names the bound function.
	fw := do(h, "GET", "/api/devices/bind-a/render", "admin-tok", nil, "")
	if fw.Code != http.StatusOK {
		t.Fatalf("forward read = %d (%s)", fw.Code, fw.Body.String())
	}
	binding, _ := jsonBody(t, fw)["binding"].(map[string]any)
	if binding == nil {
		t.Fatalf("forward read returned null binding: %s", fw.Body.String())
	}
	if bid, _ := binding["function_id"].(float64); int64(bid) != id {
		t.Errorf("forward function_id = %v, want %d", binding["function_id"], id)
	}
	if binding["name"] != "blast-fn" {
		t.Errorf("forward name = %v, want blast-fn", binding["name"])
	}

	// an unbound device reads null (not an error).
	uw := do(h, "GET", "/api/devices/bind-c/render", "admin-tok", nil, "")
	if uw.Code != http.StatusOK {
		t.Fatalf("unbound forward read = %d (%s)", uw.Code, uw.Body.String())
	}
	if b := jsonBody(t, uw)["binding"]; b != nil {
		t.Errorf("unbound device binding = %v, want null", b)
	}

	// an unknown function is a 404 (never a silent {count:0}).
	if w := do(h, "GET", "/api/functions/999999/devices", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("reverse read unknown fn = %d, want 404", w.Code)
	}

	// unbind drops the row: the device falls back to "no function" and the blast radius shrinks.
	if w := do(h, "DELETE", "/api/devices/bind-a/render", "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Errorf("unbind = %d (%s)", w.Code, w.Body.String())
	}
	fw2 := do(h, "GET", "/api/devices/bind-a/render", "admin-tok", nil, "")
	if b := jsonBody(t, fw2)["binding"]; b != nil {
		t.Errorf("post-unbind binding = %v, want null", b)
	}
	rw2 := do(h, "GET", "/api/functions/"+itoa(id)+"/devices", "admin-tok", nil, "")
	if c, _ := jsonBody(t, rw2)["count"].(float64); c != 1 {
		t.Errorf("post-unbind blast-radius count = %v, want 1", c)
	}
	// unbinding a device with no binding is a 404.
	if w := do(h, "DELETE", "/api/devices/bind-a/render", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("unbind with no binding = %d, want 404", w.Code)
	}
}
