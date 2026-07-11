package ingestcore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/faasstore"
)

// fakeFrame is an in-process FrameBackend stub (design 35 §3 replaced the render.sock transport with a
// direct method call). render lets a probe shape the returned frame; fanout captures the fan-out name.
type fakeFrame struct {
	render    func(serial, channel string) (packed []byte, status string, stale bool, wake int)
	fanout    chan string
	fanoutErr error
}

func (f *fakeFrame) Render(_ context.Context, serial, channel, _, _ string, _ bool) ([]byte, string, bool, int) {
	if f.render != nil {
		return f.render(serial, channel)
	}
	return bytes.Repeat([]byte{0x11}, packedFrameSize), "ok", false, 100
}

func (f *fakeFrame) Fanout(_ context.Context, name string, _ json.RawMessage) error {
	if f.fanout != nil {
		f.fanout <- name
	}
	return f.fanoutErr
}

// T13 — no frame backend wired (the in-process analogue of the old supervisor outage): the frame path
// serves the embedded 30000-byte unavailable frame with the short retry wake and status "unavailable" —
// never a blank/garbage body or a 500 to the panel.
func TestRequestFrame_NoBackend_T13(t *testing.T) {
	s := &Server{frame: nil, faasRetryWake: 300}
	fr := s.requestFrame(context.Background(), "sn1", "stable")
	if !bytes.Equal(fr.packed, unavailableFrame) {
		t.Errorf("no backend did not serve the unavailable frame (%d bytes)", len(fr.packed))
	}
	if fr.status != "unavailable" || fr.stale != "1" || fr.wake != 300 {
		t.Errorf("headers = status=%q stale=%q wake=%d, want unavailable/1/300", fr.status, fr.stale, fr.wake)
	}
}

// T13b — a backend that returns a NON-frame body (wrong length) is treated as an outage: the unavailable
// frame is served, never the malformed body. Red: a truncated/oversized body reaches the panel.
func TestRequestFrame_NonFrameBody_T13(t *testing.T) {
	s := &Server{faasRetryWake: 300, frame: &fakeFrame{
		render: func(_, _ string) ([]byte, string, bool, int) {
			return []byte("not a 30000-byte frame"), "ok", false, 42 // 200 but wrong size
		},
	}}
	fr := s.requestFrame(context.Background(), "sn1", "stable")
	if !bytes.Equal(fr.packed, unavailableFrame) || fr.status != "unavailable" {
		t.Errorf("non-frame body was not rejected (status=%q, %d bytes)", fr.status, len(fr.packed))
	}
}

// T13c — a well-formed backend frame is relayed verbatim with its wake/status/stale triplet.
func TestRequestFrame_Relayed(t *testing.T) {
	good := bytes.Repeat([]byte{0x7E}, packedFrameSize)
	s := &Server{faasRetryWake: 300, frame: &fakeFrame{
		render: func(_, _ string) ([]byte, string, bool, int) { return good, "stale", true, 1800 },
	}}
	fr := s.requestFrame(context.Background(), "sn1", "stable")
	if !bytes.Equal(fr.packed, good) || fr.status != "stale" || fr.stale != "1" || fr.wake != 1800 {
		t.Errorf("relay = status=%q stale=%q wake=%d, want stale/1/1800", fr.status, fr.stale, fr.wake)
	}
}

// Webhook token gate (D24.13) — the RCE-adjacent authz: only a valid per-function token fans out. Probed
// negatively: no token / wrong token → 401 (no fan-out); unknown / disabled function → 404; a valid token
// → 202 AND the backend receives the named fan-out. The compare is sha256 + constant-time.
func TestWebhook_TokenGate_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	fanout := make(chan string, 1)
	s := &Server{pool: pool, faasRetryWake: 300, frame: &fakeFrame{fanout: fanout}}

	const tokenPlain = "s3cret-webhook-token"
	sum := sha256.Sum256([]byte(tokenPlain))
	id, err := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "hook", Source: "x", TriggerType: faasstore.TriggerWebhook, WebhookTokenSHA: sum[:],
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := faasstore.SetEnabled(ctx, pool, id, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// a second, DISABLED webhook fn to prove the enabled gate.
	offID, _ := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "hook-off", Source: "x", TriggerType: faasstore.TriggerWebhook, WebhookTokenSHA: sum[:],
	})
	_ = offID

	call := func(name, token string) int {
		r := httptest.NewRequest("POST", "/tok/faas/hook/"+name, strings.NewReader(`{"k":1}`))
		if token != "" {
			r.Header.Set("X-Faas-Token", token)
		}
		w := httptest.NewRecorder()
		s.handleWebhook(w, r, name)
		return w.Code
	}

	if got := call("hook", ""); got != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", got)
	}
	if got := call("hook", "wrong-token"); got != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", got)
	}
	if got := call("does-not-exist", tokenPlain); got != http.StatusNotFound {
		t.Errorf("unknown fn = %d, want 404", got)
	}
	if got := call("hook-off", tokenPlain); got != http.StatusNotFound {
		t.Errorf("disabled webhook = %d, want 404", got)
	}
	// valid → 202, and the backend received the fan-out for THIS function.
	if got := call("hook", tokenPlain); got != http.StatusAccepted {
		t.Fatalf("valid token = %d, want 202", got)
	}
	select {
	case name := <-fanout:
		if name != "hook" {
			t.Errorf("backend fan-out name = %q, want hook", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not receive the fan-out within 2s")
	}
}
