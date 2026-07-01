package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// Webhook fan-out seam (§4.3, D24.12/D24.13): ingest has already token-validated; the supervisor
// endpoint fans the NAMED function out over every bound serial, async, replying 202. Probed negatively:
// a malformed / empty-name body is 400; an unknown name is 404; a non-webhook or DISABLED function is
// 409 (no fan-out); and a valid enabled webhook fans out — the render runs with trigger.type="webhook"
// and the POST body verbatim as trigger.payload, and the bound serial's last-good is written.
func TestWebhookFanout(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()

	mkFn := func(name string, tt faasstore.TriggerType, enabled bool) int64 {
		id, err := faasstore.Create(ctx, pool, faasstore.CreateParams{Name: name, Source: "x", TriggerType: tt})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if enabled {
			if _, err := faasstore.SetEnabled(ctx, pool, id, true); err != nil {
				t.Fatalf("enable %s: %v", name, err)
			}
		}
		return id
	}
	hookID := mkFn("hook-fn", faasstore.TriggerWebhook, true)
	if err := faasstore.BindDevice(ctx, pool, "dev-hook", hookID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	mkFn("render-fn", faasstore.TriggerRender, true)     // wrong trigger type
	mkFn("hook-off", faasstore.TriggerWebhook, false)    // right type, disabled

	seen := make(chan faasproto.RequestCtx, 4)
	render := func(_ context.Context, _ *faasstore.Function, rctx faasproto.RequestCtx, _ bool) RenderResult {
		seen <- rctx
		return RenderResult{Packed: frameOf(0x22), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
	s := testSupervisor(pool, render)

	call := func(body string) int {
		r := httptest.NewRequest("POST", "/webhook", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.handleWebhookFanout(w, r)
		return w.Code
	}

	if got := call(`not json`); got != 400 {
		t.Errorf("malformed body = %d, want 400", got)
	}
	if got := call(`{"name":""}`); got != 400 {
		t.Errorf("empty name = %d, want 400", got)
	}
	if got := call(`{"name":"does-not-exist"}`); got != 404 {
		t.Errorf("unknown fn = %d, want 404", got)
	}
	if got := call(`{"name":"render-fn"}`); got != 409 {
		t.Errorf("non-webhook fn = %d, want 409", got)
	}
	if got := call(`{"name":"hook-off"}`); got != 409 {
		t.Errorf("disabled webhook = %d, want 409", got)
	}

	if got := call(`{"name":"hook-fn","payload":{"k":1}}`); got != 202 {
		t.Fatalf("valid webhook = %d, want 202", got)
	}
	select {
	case rctx := <-seen:
		if rctx.Trigger.Type != "webhook" {
			t.Errorf("fan-out trigger.type = %q, want webhook", rctx.Trigger.Type)
		}
		if string(rctx.Trigger.Payload) != `{"k":1}` {
			t.Errorf("fan-out trigger.payload = %q, want {\"k\":1}", string(rctx.Trigger.Payload))
		}
		if rctx.Serial != "dev-hook" {
			t.Errorf("fan-out serial = %q, want dev-hook", rctx.Serial)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("async fan-out did not run within 2s")
	}

	// the fan-out wrote the bound serial's durable last-good (poll: the render stub signals before the put).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if lg, err := faasstore.LastGoodGet(ctx, pool, "dev-hook", hookID); err == nil && lg != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fan-out did not write last-good within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// the other functions must NOT have fanned out (no bindings, and they were rejected anyway).
	select {
	case extra := <-seen:
		t.Errorf("unexpected extra fan-out render: %+v", extra)
	default:
	}
}
