package faascore

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// K15 — enabled=false = frozen. The sync render path (handleRender) must treat a disabled function
// exactly like the webhook-409 and scheduler-skip already do: NEVER render inline, NEVER drive the
// worker. It serves the durable last-good flagged stale; with no last-good yet it serves the embedded
// error frame. Both branches wake on retryWake.
//
// The worker seam is a FAKE that fails the test the instant it is driven (mirrors the A31.2
// counting-stub probes, but as a hard negative): a disabled function that reaches the worker is the
// pre-K15 bug. These tests are RED against 262e8da (unchanged handleRender drives the fake worker via
// buildFrame) and GREEN after the enabled gate lands.

// neverWorker is a renderFunc that must never be called; every drive counts and fails the test.
func neverWorker(t *testing.T, calls *int) renderFunc {
	t.Helper()
	return func(_ context.Context, _ *faasstore.Function, _ faasproto.RequestCtx, _ bool) RenderResult {
		*calls++
		t.Errorf("worker driven for a DISABLED (frozen) function — enabled=false must never render")
		return RenderResult{Packed: frameOf(0x00), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
}

// postRenderResp drives handleRender via httptest and returns the recorder (body + X-Faas-* headers).
func postRenderResp(t *testing.T, s *Supervisor, serial string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"serial": serial})
	req := httptest.NewRequest(http.MethodPost, "/render", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRender(rec, req)
	return rec
}

func mkDisabledSyncFn(t *testing.T, s *Supervisor, serial string) int64 {
	t.Helper()
	ctx := context.Background()
	// Create defaults enabled=false (store.go:36) — leave it disabled: this IS the frozen function.
	fnID, err := faasstore.Create(ctx, s.pool, faasstore.CreateParams{
		Name: "frozen-fn", Source: "x", TriggerType: faasstore.TriggerRender,
		TriggerConfig: json.RawMessage(`{"mode":"sync"}`)})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	if f, err := faasstore.LoadFunction(ctx, s.pool, fnID); err != nil || f.Enabled {
		t.Fatalf("precondition: fn must be disabled, err=%v enabled=%v", err, f.Enabled)
	}
	if err := faasstore.BindDevice(ctx, s.pool, serial, fnID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return fnID
}

// Probe 1 (negative, RED against 262e8da) — a disabled sync function WITH a durable last-good serves
// that last-good flagged stale, and the fake worker is NEVER driven.
func TestHandleRenderDisabledServesStale(t *testing.T) {
	pool := faasPool(t)
	calls := 0
	s := testSupervisor(pool, nil)
	s.render = neverWorker(t, &calls)

	const serial = "DEV-FROZEN"
	fnID := mkDisabledSyncFn(t, s, serial)

	// Seed the durable last-good WITHOUT the worker (direct store write).
	good := frameOf(0x7E)
	if err := faasstore.LastGoodPut(context.Background(), pool, serial, fnID, good, "ok"); err != nil {
		t.Fatalf("seed last-good: %v", err)
	}

	rec := postRenderResp(t, s, serial)
	if calls != 0 {
		t.Fatalf("worker driven %d times for a disabled fn, want 0", calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status code %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Faas-Status"); got != "stale" {
		t.Fatalf("X-Faas-Status = %q, want stale", got)
	}
	if got := rec.Header().Get("X-Faas-Stale"); got != "1" {
		t.Fatalf("X-Faas-Stale = %q, want 1", got)
	}
	if got := rec.Header().Get("X-Faas-Wake"); got != "600" {
		t.Fatalf("X-Faas-Wake = %q, want 600 (retryWake)", got)
	}
	if out := rec.Body.Bytes(); !bytes.Equal(out, good) {
		t.Fatalf("served frame marker %#x, want the seeded last-good %#x", out[0], good[0])
	}
}

// Probe 2 (negative) — a disabled sync function WITHOUT any last-good serves the embedded error frame,
// and the fake worker is NEVER driven.
func TestHandleRenderDisabledNoLastGood(t *testing.T) {
	pool := faasPool(t)
	calls := 0
	s := testSupervisor(pool, nil)
	s.render = neverWorker(t, &calls)

	const serial = "DEV-FROZEN-BARE"
	_ = mkDisabledSyncFn(t, s, serial)

	rec := postRenderResp(t, s, serial)
	if calls != 0 {
		t.Fatalf("worker driven %d times for a disabled fn with no last-good, want 0", calls)
	}
	if got := rec.Header().Get("X-Faas-Status"); got != "error" {
		t.Fatalf("X-Faas-Status = %q, want error", got)
	}
	if got := rec.Header().Get("X-Faas-Stale"); got != "1" {
		t.Fatalf("X-Faas-Stale = %q, want 1", got)
	}
	if got := rec.Header().Get("X-Faas-Wake"); got != "600" {
		t.Fatalf("X-Faas-Wake = %q, want 600 (retryWake)", got)
	}
	if out := rec.Body.Bytes(); !bytes.Equal(out, errorFrame) {
		t.Fatalf("served frame marker %#x, want the embedded error frame", out[0])
	}
}

// Probe 3 (non-regression) — an ENABLED sync function still renders inline: the worker IS driven once
// and its frame is served fresh (status ok, not stale). Byte-identical to the pre-K15 sync path.
func TestHandleRenderEnabledRendersInline(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	calls := 0
	render := func(_ context.Context, _ *faasstore.Function, _ faasproto.RequestCtx, _ bool) RenderResult {
		calls++
		return RenderResult{Packed: frameOf(0xEE), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
	s := testSupervisor(pool, render)

	const serial = "DEV-LIVE"
	fnID, err := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "live-fn", Source: "x", TriggerType: faasstore.TriggerRender,
		TriggerConfig: json.RawMessage(`{"mode":"sync"}`)})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	if _, err := faasstore.SetEnabled(ctx, pool, fnID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, serial, fnID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	rec := postRenderResp(t, s, serial)
	if calls != 1 {
		t.Fatalf("enabled fn drove the worker %d times, want 1 (inline render)", calls)
	}
	if got := rec.Header().Get("X-Faas-Status"); got != "ok" {
		t.Fatalf("X-Faas-Status = %q, want ok", got)
	}
	if got := rec.Header().Get("X-Faas-Stale"); got != "0" {
		t.Fatalf("X-Faas-Stale = %q, want 0", got)
	}
	if out := rec.Body.Bytes(); !bytes.Equal(out, frameOf(0xEE)) {
		t.Fatalf("served frame marker %#x, want the freshly rendered 0xEE", out[0])
	}
}
