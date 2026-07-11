package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/templatestore"
)

// testPreviewHandler mounts GET /api/templates/{id}/preview with an INJECTED packed-frame producer
// (no supervisor in the unit test). The single-slot budget + fixtures are the production ones.
func testPreviewHandler(pool *pgxpool.Pool, render func(context.Context, string) ([]byte, error)) (http.Handler, *previewRenderer) {
	fixtures, err := templatestore.PreviewFixtures()
	if err != nil {
		panic(err)
	}
	rndr := &previewRenderer{
		pool:     pool,
		sem:      make(chan struct{}, 1),
		fixtures: fixtures,
		render:   render,
	}
	mux := http.NewServeMux()
	mux.Handle("GET /api/templates/{id}/preview", adminhttp.Auth(pool)(http.HandlerFunc(templatePreviewHandlers{pool: pool, rndr: rndr}.preview)))
	return adminhttp.WithRequestID(mux), rndr
}

// blackFrame is a valid 30000-byte packed frame (all code 0) — the stub render's stand-in for a
// supervisor-produced (or error-frame) packed frame.
func blackFrame() []byte { return make([]byte, bwry.PackedSize) }

// TestPreviewBudget_SingleSlot is the budget probe (§4.5a): EVERY live preview render goes through the
// one-slot semaphore, so peak concurrency is exactly 1 no matter how many renders fire at once. The RED
// baseline in the same test calls the identical render closure WITHOUT the budget and reaches peak>1 —
// proving the semaphore, not the render itself, is the limiter. Remove `p.sem <- struct{}{}` / its
// receive in produce() and the green assertion fails.
func TestPreviewBudget_SingleSlot(t *testing.T) {
	const workers = 8
	newProbe := func() (func(context.Context, string) ([]byte, error), *int32) {
		var cur, peak int32
		return func(context.Context, string) ([]byte, error) {
			c := atomic.AddInt32(&cur, 1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&cur, -1)
			return blackFrame(), nil
		}, &peak
	}

	// GREEN — through the single-slot budget.
	green, greenPeak := newProbe()
	p := &previewRenderer{sem: make(chan struct{}, 1), fixtures: map[string][]byte{}, render: green}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := p.produce(context.Background(), fmt.Sprintf("op/%d", n), templatestore.KindRenderFn, "src"); err != nil {
				t.Errorf("produce: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(greenPeak); got != 1 {
		t.Fatalf("BUDGET BREACHED: peak concurrent preview renders = %d, want 1", got)
	}

	// RED baseline — the same render with NO budget reaches peak>1 (proves the slot is what limits it).
	red, redPeak := newProbe()
	var wg2 sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg2.Add(1)
		go func() { defer wg2.Done(); _, _ = red(context.Background(), "src") }()
	}
	wg2.Wait()
	if got := atomic.LoadInt32(redPeak); got <= 1 {
		t.Fatalf("red baseline peak = %d, want >1 (probe cannot distinguish an enforced budget)", got)
	}
}

// TestPreviewETag304_DB is the cache + ETag probe (§4.5a, W-A33.5a probe b): the FIRST GET renders
// on-demand and 200s with ETag "<id>-<version>"; the SECOND GET with If-None-Match 304s WITHOUT a body
// and WITHOUT a second render (the durable cache hit). Red: no ETag/304 handling ⇒ the second GET
// re-renders and 200s.
func TestPreviewETag304_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	if _, err := templatestore.SeedBuiltins(context.Background(), pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, version := builtinRenderFnWithoutFixture(t, pool) // clock: no fixture ⇒ live render

	var renders int32
	h, _ := testPreviewHandler(pool, func(context.Context, string) ([]byte, error) {
		atomic.AddInt32(&renders, 1)
		return blackFrame(), nil
	})
	path := fmt.Sprintf("/api/templates/%d/preview", id)

	w1 := do(h, "GET", path, "admin-tok", nil, "")
	if w1.Code != http.StatusOK {
		t.Fatalf("first GET = %d, want 200 (%s)", w1.Code, w1.Body.String())
	}
	etag := w1.Header().Get("ETag")
	if want := fmt.Sprintf(`"%d-%d"`, id, version); etag != want {
		t.Fatalf("ETag = %q, want %q", etag, want)
	}
	if w1.Body.Len() == 0 {
		t.Fatal("first GET body empty, want PNG bytes")
	}
	if cc := w1.Header().Get("Cache-Control"); cc == "" {
		t.Errorf("Cache-Control missing on 200")
	}

	// second GET with If-None-Match
	req := httptest.NewRequest("GET", path, nil)
	req = req.WithContext(adminhttp.WithListenerOrigin(req.Context(), adminhttp.OriginLoopback))
	req.Header.Set("Authorization", "Bearer admin-tok")
	req.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusNotModified {
		t.Fatalf("second GET = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Fatalf("304 body = %d bytes, want 0", w2.Body.Len())
	}
	if got := atomic.LoadInt32(&renders); got != 1 {
		t.Fatalf("render count = %d across two GETs, want 1 (cache/304 must not re-render)", got)
	}
}

// TestPreviewFixturePolicy_DB is the fixture-policy probe (§4.5a, probe a): a fetch builtin WITH a
// preview_fixture serves the curated PNG and NEVER enters the live-render path; a fixture-less builtin
// enters the live-render path (whose supervisor arm would substitute the error frame under egress-deny).
func TestPreviewFixturePolicy_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	if _, err := templatestore.SeedBuiltins(context.Background(), pool); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fixtures, _ := templatestore.PreviewFixtures()
	if len(fixtures) == 0 {
		t.Fatal("no preview fixtures in catalog — the fetch-builtin card would show the error frame")
	}

	var rendered []string
	h, _ := testPreviewHandler(pool, func(_ context.Context, source string) ([]byte, error) {
		rendered = append(rendered, source)
		return blackFrame(), nil // stands in for the supervisor's error-frame substitution
	})

	// external-image: fetch builtin WITH fixture ⇒ fixture bytes, render NOT entered.
	fxID, _ := builtinByName(t, pool, "builtin/external-image")
	w := do(h, "GET", fmt.Sprintf("/api/templates/%d/preview", fxID), "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("fixture builtin GET = %d, want 200", w.Code)
	}
	if got, want := w.Body.Bytes(), fixtures["builtin/external-image"]; string(got) != string(want) {
		t.Fatalf("fixture builtin served %d bytes, want the %d-byte fixture", len(got), len(want))
	}
	if len(rendered) != 0 {
		t.Fatalf("live-render path ENTERED for a fixture builtin (%v) — must be skipped", rendered)
	}

	// clock: builtin WITHOUT fixture ⇒ live-render path entered, error-frame PNG served.
	ckID, _ := builtinByName(t, pool, "builtin/clock")
	w = do(h, "GET", fmt.Sprintf("/api/templates/%d/preview", ckID), "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("no-fixture builtin GET = %d, want 200", w.Code)
	}
	if len(rendered) != 1 {
		t.Fatalf("live-render path entered %d times for the no-fixture builtin, want 1", len(rendered))
	}
	wantPNG, _ := templatestore.PackedToPNG(blackFrame())
	if string(w.Body.Bytes()) != string(wantPNG) {
		t.Fatalf("no-fixture builtin did not serve the rendered (error-frame) PNG")
	}
}

// TestPreviewIncludeSource_DB is the include=source probe (§4.5a, probe d): only berry_snippet rows
// carry `source`; a render_fn does not. limit>50 with include=source is CAPPED to 50 (design "≤50
// gedeckelt"). Red: an uncapped list returns all rows / a render_fn leaks its JS body.
func TestPreviewIncludeSource_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)

	// 60 berry_snippets (> the 50 cap) + one render_fn.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO templates (name, kind, source, builtin)
		 SELECT 'snip-'||g, 'berry_snippet', 'set_wifi("a","b")', false FROM generate_series(1,60) g`); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO templates (name, kind, source, builtin) VALUES ('rf1','render_fn','export default async()=>({})',false)`); err != nil {
		t.Fatalf("render_fn insert: %v", err)
	}

	h := testTemplateHandler(pool)

	// limit=200 with include=source ⇒ capped to 50 rows.
	w := do(h, "GET", "/api/templates?include=source&limit=200", "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Templates []struct {
			Kind   string `json:"kind"`
			Source string `json:"source"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Templates) != previewSourceListMax {
		t.Fatalf("include=source&limit=200 returned %d rows, want %d (capped)", len(resp.Templates), previewSourceListMax)
	}
	for i, tm := range resp.Templates {
		if tm.Kind == "berry_snippet" && tm.Source == "" {
			t.Fatalf("row %d berry_snippet has empty source (include=source must carry it)", i)
		}
		if tm.Kind == "render_fn" && tm.Source != "" {
			t.Fatalf("row %d render_fn leaked source %q (only snippets carry it)", i, tm.Source)
		}
	}

	// Without include=source the source field is absent even for snippets.
	w = do(h, "GET", "/api/templates?limit=5", "admin-tok", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("plain list = %d, want 200", w.Code)
	}
	if got := w.Body.String(); jsonHasSourceField(got) {
		t.Fatalf("plain list leaked a source field: %s", truncBody(got))
	}
}

// TestPreviewOperatorAdminOnly_DB — an operator-authored template's preview is admin-only (E-A33-6):
// a read-only key is 403, never a render.
func TestPreviewOperatorAdminOnly_DB(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "admin-tok", true)
	seedOperator(t, pool, "ro-tok", false)
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO templates (name, kind, source, builtin) VALUES ('op-rf','render_fn','export default async()=>({})',false) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	h, _ := testPreviewHandler(pool, func(context.Context, string) ([]byte, error) { return blackFrame(), nil })
	if w := do(h, "GET", fmt.Sprintf("/api/templates/%d/preview", id), "ro-tok", nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("operator preview with ro key = %d, want 403", w.Code)
	}
	if w := do(h, "GET", fmt.Sprintf("/api/templates/%d/preview", id), "admin-tok", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("operator preview with admin key = %d, want 200", w.Code)
	}
}

// --- helpers ---

func builtinRenderFnWithoutFixture(t *testing.T, pool *pgxpool.Pool) (int64, int) {
	t.Helper()
	var id int64
	var version int
	if err := pool.QueryRow(context.Background(),
		`SELECT id, version FROM templates WHERE name = 'builtin/clock'`).Scan(&id, &version); err != nil {
		t.Fatalf("locate builtin/clock: %v", err)
	}
	return id, version
}

func builtinByName(t *testing.T, pool *pgxpool.Pool, name string) (int64, int) {
	t.Helper()
	var id int64
	var version int
	if err := pool.QueryRow(context.Background(),
		`SELECT id, version FROM templates WHERE name = $1`, name).Scan(&id, &version); err != nil {
		t.Fatalf("locate %s: %v", name, err)
	}
	return id, version
}

func jsonHasSourceField(body string) bool {
	var resp struct {
		Templates []map[string]json.RawMessage `json:"templates"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return false
	}
	for _, row := range resp.Templates {
		if _, ok := row["source"]; ok {
			return true
		}
	}
	return false
}
