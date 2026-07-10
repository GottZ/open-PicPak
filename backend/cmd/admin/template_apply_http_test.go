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

// T-apply-wrong-kind — applying a playlist_preset is a defined 422 unsupported_kind (A29 owns its
// materialisation, §9), never a panic path (§5.2). render_fn and berry_snippet are handled (W4/W5). Red: a
// dispatch that falls through / type-asserts a nil branch panics or 500s instead of a clean 422.
func TestTemplateApplyWrongKind_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	preset := createTemplate(t, h, `{"name":"wk-preset","kind":"playlist_preset","source":"{}"}`)
	w := do(h, "POST", "/api/templates/"+itoa(preset)+"/apply", "admin-tok",
		strings.NewReader(`{"target_serials":["*"]}`), "application/json")
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("apply playlist_preset = %d, want 422 (%s)", w.Code, truncBody(w.Body.String()))
	}
	if code, _ := jsonBody(t, w)["code"].(string); code != "unsupported_kind" {
		t.Errorf("apply playlist_preset code = %q, want unsupported_kind", code)
	}
}

// T-apply-render-stamp — a render_fn apply mints a faas_functions row that carries the template_id
// provenance stamp AND the three trust-profile fields (egress_allow / secret_bindings / trigger_config)
// copied from the template row, source-substituted, enabled=false. Asserted on the DB row. Red: without the
// trust-profile passthrough the function fetches with an empty allow-list and the egress proxy hard-blocks
// it (a dead Deliverable-4 function); without the stamp its provenance is lost.
func TestTemplateApplyRenderStamp_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	id := createTemplate(t, h, `{"name":"stamp-render","kind":"render_fn","source":"fetch({{url}})",`+
		`"egress_allow":["example.com"],"secret_bindings":["API_KEY"],"trigger_config":{"mode":"sync","ttl_s":60}}`)
	w := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok",
		strings.NewReader(`{"params":{"url":"example.com/a"}}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("render apply = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	fnID := int64(jsonBody(t, w)["id"].(float64))

	var tmplID *int64
	var egress, secrets []string
	var trigger, source string
	var enabled bool
	if err := pool.QueryRow(context.Background(),
		`SELECT template_id, egress_allow, secret_bindings, trigger_config::text, source, enabled
		   FROM faas_functions WHERE id = $1`, fnID).
		Scan(&tmplID, &egress, &secrets, &trigger, &source, &enabled); err != nil {
		t.Fatalf("read minted function: %v", err)
	}
	if tmplID == nil || *tmplID != id {
		t.Errorf("template_id stamp = %v, want %d", tmplID, id)
	}
	if len(egress) != 1 || egress[0] != "example.com" {
		t.Errorf("egress_allow = %v, want [example.com]", egress)
	}
	if len(secrets) != 1 || secrets[0] != "API_KEY" {
		t.Errorf("secret_bindings = %v, want [API_KEY]", secrets)
	}
	if !strings.Contains(trigger, "sync") { // JSONB re-serialises with spaces; the value is what matters
		t.Errorf("trigger_config = %q, want it to carry the template's sync config", trigger)
	}
	if source != "fetch(example.com/a)" {
		t.Errorf("source = %q, want the substituted fetch(example.com/a)", source)
	}
	if enabled {
		t.Errorf("minted function enabled=true, want false (operator activates deliberately)")
	}
}

// T-apply-render-collision — repeatability at fleet scale: a derived-name apply of the SAME template twice
// mints two distinct functions (the second gets a -2 suffix via the collision retry), while an explicit
// fn_name that collides is a terminal 409 function_name_taken. Red: without the retry the second derived
// apply dies on the name UNIQUE (409) instead of minting a fresh function.
func TestTemplateApplyRenderCollision_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)

	id := createTemplate(t, h, `{"name":"coll-render","kind":"render_fn","source":"export default async()=>({})"}`)
	path := "/api/templates/" + itoa(id) + "/apply"

	w1 := do(h, "POST", path, "admin-tok", strings.NewReader(`{}`), "application/json")
	w2 := do(h, "POST", path, "admin-tok", strings.NewReader(`{}`), "application/json")
	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("derived double-apply = %d / %d, want 200 / 200 (%s)", w1.Code, w2.Code, w2.Body.String())
	}
	n1, _ := jsonBody(t, w1)["name"].(string)
	n2, _ := jsonBody(t, w2)["name"].(string)
	if n1 == n2 {
		t.Errorf("derived names collided: both %q (retry did not fire)", n1)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM faas_functions WHERE template_id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("count functions: %v", err)
	}
	if count != 2 {
		t.Errorf("template minted %d functions, want 2", count)
	}

	// explicit fn_name collision is terminal 409.
	e1 := do(h, "POST", path, "admin-tok", strings.NewReader(`{"fn_name":"explicit-fn"}`), "application/json")
	if e1.Code != http.StatusOK {
		t.Fatalf("explicit apply = %d, want 200 (%s)", e1.Code, e1.Body.String())
	}
	e2 := do(h, "POST", path, "admin-tok", strings.NewReader(`{"fn_name":"explicit-fn"}`), "application/json")
	if e2.Code != http.StatusConflict {
		t.Errorf("explicit name collision = %d, want 409 (%s)", e2.Code, e2.Body.String())
	}
	if code, _ := jsonBody(t, e2)["code"].(string); code != "function_name_taken" {
		t.Errorf("explicit collision code = %q, want function_name_taken", code)
	}
}

// T-apply-render-bind-n1 — the n:1 fleet bind: applying with bind + N serials mints ONE function and binds
// all N serials to it (the render binding has no "*" fanout, §4.3). Red: a per-serial function mint would
// leave N functions, or a missing bind loop would leave 0 bindings.
func TestTemplateApplyRenderBindN1_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	h := testTemplateHandler(pool)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('bn-a','stable'),('bn-b','stable'),('bn-c','stable')`)

	id := createTemplate(t, h, `{"name":"bind-render","kind":"render_fn","source":"export default async()=>({})"}`)
	w := do(h, "POST", "/api/templates/"+itoa(id)+"/apply", "admin-tok",
		strings.NewReader(`{"bind":true,"target_serials":["bn-a","bn-b","bn-c"]}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("bind apply = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	fnID := int64(jsonBody(t, w)["id"].(float64))

	var funcs int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM faas_functions WHERE template_id = $1`, id).Scan(&funcs); err != nil {
		t.Fatalf("count functions: %v", err)
	}
	if funcs != 1 {
		t.Errorf("n:1 apply minted %d functions, want 1", funcs)
	}
	var binds int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM device_render_binding WHERE function_id = $1`, fnID).Scan(&binds); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if binds != 3 {
		t.Errorf("n:1 apply produced %d bindings, want 3", binds)
	}
}
