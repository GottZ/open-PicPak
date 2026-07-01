package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/faasstore"
)

// T13 — supervisor outage: the frame path serves the embedded 30000-byte unavailable frame with a short
// retry wake and status "unavailable" — never a blank body, a garbage body, or a 500 to the panel. Probed
// by pointing the render client at a socket with nothing listening (no DB needed).
func TestRequestFrame_SupervisorOutage_T13(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "dead.sock") // nothing is listening here
	s := &server{faasClient: newFaasClient(dead), faasRetryWake: 300}
	fr := s.requestFrame(context.Background(), "sn1", "stable")
	if !bytes.Equal(fr.packed, unavailableFrame) {
		t.Errorf("outage did not serve the unavailable frame (%d bytes)", len(fr.packed))
	}
	if fr.status != "unavailable" || fr.stale != "1" || fr.wake != 300 {
		t.Errorf("outage headers = status=%q stale=%q wake=%d, want unavailable/1/300", fr.status, fr.stale, fr.wake)
	}
}

// T13b — a supervisor that returns a NON-frame body (wrong length) is also treated as an outage: the
// unavailable frame is served, never the malformed body. Red: a truncated/oversized body reaches the panel.
func TestRequestFrame_NonFrameBody_T13(t *testing.T) {
	sock, stop := mockSupervisor(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a 30000-byte frame")) // 200 but wrong size
	})
	defer stop()
	s := &server{faasClient: newFaasClient(sock), faasRetryWake: 300}
	fr := s.requestFrame(context.Background(), "sn1", "stable")
	if !bytes.Equal(fr.packed, unavailableFrame) || fr.status != "unavailable" {
		t.Errorf("non-frame body was not rejected (status=%q, %d bytes)", fr.status, len(fr.packed))
	}
}

// Webhook token gate (D24.13) — the RCE-adjacent authz: only a valid per-function token fans out. Probed
// negatively: no token / wrong token → 401 (no fan-out); unknown / disabled function → 404; a valid token
// → 202 AND the supervisor receives the named fan-out. The compare is sha256 + constant-time.
func TestWebhook_TokenGate_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	fanout := make(chan string, 1)
	sock, stop := mockSupervisor(t, func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Name    string          `json:"name"`
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		fanout <- b.Name
		w.WriteHeader(http.StatusAccepted)
	})
	defer stop()
	s := &server{pool: pool, faasClient: newFaasClient(sock), faasRetryWake: 300}

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
	// valid → 202, and the supervisor received the fan-out for THIS function.
	if got := call("hook", tokenPlain); got != http.StatusAccepted {
		t.Fatalf("valid token = %d, want 202", got)
	}
	select {
	case name := <-fanout:
		if name != "hook" {
			t.Errorf("supervisor fan-out name = %q, want hook", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not receive the fan-out within 2s")
	}
}

// mockSupervisor starts a tiny HTTP server on a fresh UDS and returns its socket path + a stop func.
func mockSupervisor(t *testing.T, handler http.HandlerFunc) (sock string, stop func()) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "render.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	return sock, func() { _ = srv.Close() }
}
