package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// createTemplate mints a template through the production write path (POST /api/templates) and returns its
// id — fixtures ride the real handler, never a hand INSERT (W10).
func createTemplate(t *testing.T, h http.Handler, body string) int64 {
	t.Helper()
	w := do(h, "POST", "/api/templates", "admin-tok", strings.NewReader(body), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("create template = %d (%s)", w.Code, w.Body.String())
	}
	return int64(jsonBody(t, w)["id"].(float64))
}

func pendingCount(t *testing.T, pool *pgxpool.Pool, serial string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM command_queue WHERE serial = $1 AND NOT applied`, serial).Scan(&n); err != nil {
		t.Fatalf("count command_queue: %v", err)
	}
	return n
}

// T-apply-gate — POST /api/templates/{id}/apply inherits the admin gate: 401 without a bearer, 403 for a
// read-only key, past-the-gate for admin. /apply is RCE-equivalent, so it must gate exactly like the
// mutations. Red: an /apply mounted without RequireAdmin answers past-the-gate for the read-only key.
func TestTemplateApplyGating_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	h := testTemplateHandler(pool)

	if w := do(h, "POST", "/api/templates/1/apply", "", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("apply no bearer = %d, want 401", w.Code)
	}
	if w := do(h, "POST", "/api/templates/1/apply", "ro-tok", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("apply readonly = %d, want 403", w.Code)
	}
	// admin past the gate: an unknown id → 404 (proves NOT 401/403).
	if w := do(h, "POST", "/api/templates/99999/apply", "admin-tok",
		strings.NewReader(`{"target_serials":["*"]}`), "application/json"); w.Code != http.StatusNotFound {
		t.Errorf("apply unknown id (admin) = %d, want 404", w.Code)
	}
}

// T-apply-berry-octet — the poison-pill gate: a berry_snippet whose STORED source is under 8191 bytes but
// whose SUBSTITUTED source crosses the byte cap must be rejected 422 berry_too_long. The param is multibyte
// (€ = 3 bytes) so char_length would admit it — the check is octets (W18). Red: a pre-substitution-only
// length check (on the stored row) passes it through to the enqueue, which then errors as a generic
// invalid_script — so the SPECIFIC berry_too_long code proves the post-substitution guard fired.
func TestTemplateApplyBerryOctet_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('octetsn','stable')`)

	// stored source is tiny; {{v}} expands to 4000*'€' = 12000 bytes -> substituted > 8191.
	id := createTemplate(t, h, `{"name":"octet-berry","kind":"berry_snippet","source":"x={{v}}"}`)
	big := strings.Repeat("€", 4000)
	body := `{"target_serials":["octetsn"],"params":{"v":"` + big + `"}}`
	w := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok", strings.NewReader(body), "application/json")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("over-cap apply = %d, want 422 (%s)", w.Code, truncBody(w.Body.String()))
	}
	if code, _ := jsonBody(t, w)["code"].(string); code != "berry_too_long" {
		t.Errorf("over-cap apply code = %q, want berry_too_long (post-substitution guard did not fire)", code)
	}
	if n := pendingCount(t, pool, "octetsn"); n != 0 {
		t.Errorf("rejected apply still enqueued %d rows, want 0", n)
	}

	// control: the same template with a small param applies (substitution happened, under cap).
	ok := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok",
		strings.NewReader(`{"target_serials":["octetsn"],"params":{"v":"hi"}}`), "application/json")
	if ok.Code != http.StatusOK {
		t.Fatalf("in-cap apply = %d, want 200 (%s)", ok.Code, ok.Body.String())
	}
	var script string
	if err := pool.QueryRow(context.Background(),
		`SELECT script FROM command_queue WHERE serial='octetsn' ORDER BY seq DESC LIMIT 1`).Scan(&script); err != nil {
		t.Fatalf("read enqueued script: %v", err)
	}
	if script != "x=hi" {
		t.Errorf("substitution wrong: script = %q, want x=hi", script)
	}
}

// T-apply-berry-dedup — the K4 fleet double-effect guard reaches through /apply: a double-clicked identical
// (template, params, serial) apply dedups to ONE pending row (derived idempotency key), while a changed
// param set carries a different key → a second row. Red: without the derived key the append-only queue
// double-enqueues, and every device runs the snippet twice.
func TestTemplateApplyBerryDedup_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dedupsn','stable')`)

	id := createTemplate(t, h, `{"name":"dedup-berry","kind":"berry_snippet","source":"set_url({{u}})"}`)
	apply := func(params string) {
		body := `{"target_serials":["dedupsn"],"params":` + params + `}`
		w := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok", strings.NewReader(body), "application/json")
		if w.Code != http.StatusOK {
			t.Fatalf("apply %s = %d (%s)", params, w.Code, w.Body.String())
		}
	}
	apply(`{"u":"a"}`)
	apply(`{"u":"a"}`) // identical → dedup
	if n := pendingCount(t, pool, "dedupsn"); n != 1 {
		t.Errorf("identical double-apply → %d pending rows, want 1 (K4 dedup)", n)
	}
	apply(`{"u":"b"}`) // changed params → new key → new row
	if n := pendingCount(t, pool, "dedupsn"); n != 2 {
		t.Errorf("changed-param apply → %d pending rows, want 2 (new key)", n)
	}
}

// T-apply-wrong-kind — applying to a non-berry kind on this wave is a defined 422 unsupported_kind, never a
// panic path (§5.2). render_fn lands in W5; playlist_preset is A29's materialisation (§9). Red: a dispatch
// that falls through / type-asserts a nil branch panics or 500s instead of a clean 422.
func TestTemplateApplyWrongKind_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	render := createTemplate(t, h, `{"name":"wk-render","kind":"render_fn","source":"export default async()=>({})"}`)
	preset := createTemplate(t, h, `{"name":"wk-preset","kind":"playlist_preset","source":"{}"}`)
	for _, id := range []int64{render, preset} {
		w := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok",
			strings.NewReader(`{"target_serials":["*"]}`), "application/json")
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("apply id %d (non-berry) = %d, want 422 (%s)", id, w.Code, truncBody(w.Body.String()))
		}
		if code, _ := jsonBody(t, w)["code"].(string); code != "unsupported_kind" {
			t.Errorf("apply id %d code = %q, want unsupported_kind", id, code)
		}
	}
}
