package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/operator"
	"github.com/open-picpak/backend/internal/telemetry"
)

func strptr(s string) *string { return &s }

// newTestHub builds a hub with injected fake roster + authenticate seams (no DB).
func newTestHub(life context.Context, rows []devicestore.Row, auth func(context.Context, string) (operator.AuthResult, bool, error), cfg sseConfig) *sseHub {
	return &sseHub{
		life:         life,
		cfg:          cfg,
		roster:       func(context.Context) ([]devicestore.Row, error) { return rows, nil },
		authenticate: auth,
		subs:         map[*sseSub]struct{}{},
	}
}

func okAuth(context.Context, string) (operator.AuthResult, bool, error) {
	return operator.AuthResult{KeyID: 1, IsAdmin: true, Label: "test"}, true, nil
}

// --- sseWriter frame format ---

func TestSSEWriterFrameFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec, time.Second)
	if err := sw.event("snapshot", "42", []byte(`{"a":1}`)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if err := sw.ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := sw.event("devices", "", []byte(`{"op":"remove"}`)); err != nil {
		t.Fatalf("event no-id: %v", err)
	}
	got := rec.Body.String()
	want := "id: 42\nevent: snapshot\ndata: {\"a\":1}\n\n" +
		": ping\n\n" +
		"event: devices\ndata: {\"op\":\"remove\"}\n\n"
	if got != want {
		t.Errorf("frame format:\n got %q\nwant %q", got, want)
	}
}

// TestSSEFrameIntegrityEscapesNewlines is the HUB invariant (design 19 §4.5): a
// device-supplied string (here a malicious label) cannot inject a premature
// blank-line frame boundary, because the payload is json.Marshal'd (newlines
// escaped) before it becomes the single data: line. Negatively probed: a label
// crafted to forge "\n\nevent: injected" must NOT split the frame.
func TestSSEFrameIntegrityEscapesNewlines(t *testing.T) {
	evil := "ok\n\nevent: injected\ndata: forged"
	d := rosterDelta{Op: "upsert", Device: devicestore.Row{Serial: "D2XXXXR", Label: strptr(evil), Channel: "stable"}}
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec, time.Second)
	// marshal exactly as broadcastDelta does (HUB-side json.Marshal), then frame it.
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := sw.event("devices", "", data); err != nil {
		t.Fatalf("event: %v", err)
	}
	out := rec.Body.String()
	// Exactly ONE frame terminator — the evil label did not forge a second.
	if n := strings.Count(out, "\n\n"); n != 1 {
		t.Errorf("frame split into %d (want 1) — newline not escaped:\n%q", n, out)
	}
	// The literal "event: injected" must not appear at line start (would be a
	// forged event); it survives only inside the escaped JSON string.
	if strings.Contains(out, "\nevent: injected") {
		t.Errorf("forged event line leaked through:\n%q", out)
	}
}

// --- T5: rolling write-deadline outlives an absolute WriteTimeout ---

func TestSSEWriterOutlivesServerWriteTimeout(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sw := newSSEWriter(w, 400*time.Millisecond) // window > server WriteTimeout
		deadline := time.After(900 * time.Millisecond) // 3x the server WriteTimeout
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-deadline:
				_ = sw.event("devices", "1", []byte(`{"op":"upsert","ok":"made-it"}`))
				return
			case <-tk.C:
				if sw.ping() != nil {
					return
				}
			}
		}
	}))
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body (connection killed by WriteTimeout?): %v", err)
	}
	// Without the per-frame SetWriteDeadline roll, the 300ms absolute WriteTimeout
	// kills the write at 300ms and "made-it" (sent at 900ms) never arrives.
	if !strings.Contains(string(body), "made-it") {
		t.Errorf("payload missing — write-deadline not rolled? got %d bytes: %q", len(body), string(body))
	}
}

// --- T6: flush reaches the base writer through the Unwrap() chain ---

type unwrapWriter struct{ http.ResponseWriter }

func (u unwrapWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

type noUnwrapWriter struct{ http.ResponseWriter }

func TestSSEFlushThroughUnwrap(t *testing.T) {
	// A wrapper that implements Unwrap() → ResponseController.Flush walks the chain
	// to the flushable recorder → event() succeeds.
	rec := httptest.NewRecorder()
	sw := newSSEWriter(unwrapWriter{rec}, time.Second)
	if err := sw.event("devices", "", []byte(`{}`)); err != nil {
		t.Errorf("flush through Unwrap() chain must succeed, got: %v", err)
	}

	// A wrapper WITHOUT Unwrap() dead-ends the controller → Flush errors. This is
	// the negative probe pinning the standing convention (any cmd/admin middleware
	// wrapping the writer MUST implement Unwrap, else the SSE flush silently breaks).
	rec2 := httptest.NewRecorder()
	sw2 := newSSEWriter(noUnwrapWriter{rec2}, time.Second)
	if err := sw2.event("devices", "", []byte(`{}`)); err == nil {
		t.Error("a writer wrapper without Unwrap() must fail to flush — convention not enforced")
	}
}

// --- diffRoster: identity diff, last_seen is NOT a delta trigger ---

func TestDiffRoster(t *testing.T) {
	t1 := time.Unix(1000, 0)
	t2 := time.Unix(2000, 0)
	base := map[string]devicestore.Row{
		"D2AAAA1": {Serial: "D2AAAA1", Label: strptr("A"), Channel: "stable", LastSeen: &t1, Bonded: false},
	}

	// new serial → upsert
	cur := map[string]devicestore.Row{
		"D2AAAA1": base["D2AAAA1"],
		"D2BBBB2": {Serial: "D2BBBB2", Channel: "beta"},
	}
	d := diffRoster(base, cur)
	if len(d) != 1 || d[0].Op != "upsert" || d[0].Device.Serial != "D2BBBB2" {
		t.Errorf("new serial: want one upsert of D2BBBB2, got %+v", d)
	}

	// last_seen-only change → NO delta (the critical design invariant)
	lastSeenOnly := map[string]devicestore.Row{
		"D2AAAA1": {Serial: "D2AAAA1", Label: strptr("A"), Channel: "stable", LastSeen: &t2, Bonded: false},
	}
	if d := diffRoster(base, lastSeenOnly); len(d) != 0 {
		t.Errorf("last_seen-only change must NOT emit a delta (it churns the roster), got %+v", d)
	}

	// identity change (bonded flip) → upsert
	bonded := map[string]devicestore.Row{
		"D2AAAA1": {Serial: "D2AAAA1", Label: strptr("A"), Channel: "stable", LastSeen: &t1, Bonded: true},
	}
	if d := diffRoster(base, bonded); len(d) != 1 || d[0].Op != "upsert" {
		t.Errorf("bonded flip must emit an upsert, got %+v", d)
	}

	// label change → upsert
	relabel := map[string]devicestore.Row{
		"D2AAAA1": {Serial: "D2AAAA1", Label: strptr("renamed"), Channel: "stable", LastSeen: &t1},
	}
	if d := diffRoster(base, relabel); len(d) != 1 || d[0].Op != "upsert" {
		t.Errorf("label change must emit an upsert, got %+v", d)
	}

	// vanished serial → remove
	if d := diffRoster(base, map[string]devicestore.Row{}); len(d) != 1 || d[0].Op != "remove" || d[0].Device.Serial != "D2AAAA1" {
		t.Errorf("vanished serial must emit a remove, got %+v", d)
	}

	// identical → no delta
	if d := diffRoster(base, map[string]devicestore.Row{"D2AAAA1": base["D2AAAA1"]}); len(d) != 0 {
		t.Errorf("identical roster must emit nothing, got %+v", d)
	}
}

// --- hub: connection cap ---

func TestSSEHubCapRefusesOverLimit(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := sseConfig{tick: time.Second, ping: time.Second, reauth: time.Second, writeWindow: time.Second, maxConn: 2}
	h := newTestHub(life, nil, okAuth, cfg)

	s1, ok1 := h.subscribe()
	_, ok2 := h.subscribe()
	_, ok3 := h.subscribe()
	if !ok1 || !ok2 {
		t.Fatal("first two subscribes (cap=2) must succeed")
	}
	if ok3 {
		t.Error("third subscribe must be refused at cap=2 (→ 429)")
	}
	h.unsubscribe(s1)
	if _, ok := h.subscribe(); !ok {
		t.Error("a freed slot must be reusable after unsubscribe")
	}
}

// --- hub: one broadcast loop fans a roster change to many connections ---

func TestSSEHubDeltaFanOut(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := []devicestore.Row{{Serial: "D2AAAA1", Channel: "stable"}}
	cfg := sseConfig{tick: 10 * time.Millisecond, ping: time.Second, reauth: time.Second, writeWindow: time.Second, maxConn: 8}
	h := newTestHub(life, rows, okAuth, cfg)

	sub, ok := h.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	var mu sync.Mutex
	devicesFrames := 0
	go func() {
		for f := range sub.ch {
			if f.name == "devices" {
				mu.Lock()
				devicesFrames++
				mu.Unlock()
			}
		}
	}()
	// First tick diffs the roster against the empty baseline → one upsert. A
	// stable roster thereafter emits nothing more.
	waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return devicesFrames == 1 })
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	got := devicesFrames
	mu.Unlock()
	if got != 1 {
		t.Errorf("stable roster after first tick: got %d devices frames, want exactly 1 (no churn)", got)
	}
}

// The telemetry producer fans onto the SAME hub as the roster/log producers (D22.7): a tick with one
// telemetry delta broadcasts exactly one `telemetry` frame, and a sparse stream (nothing new after) adds
// no churn. The fake producer mimics prime→diff→quiet; this proves the tickTelemetry → broadcastJSON
// wiring without a DB (the watermark query itself is covered by the telemetry package's T7 DB test).
func TestSSEHubTelemetryFanOut(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := sseConfig{tick: 10 * time.Millisecond, ping: time.Second, reauth: time.Second, writeWindow: time.Second, maxConn: 8}
	h := newTestHub(life, nil, okAuth, cfg)

	var cmu sync.Mutex
	calls := 0
	h.telemetry = func(context.Context, telemetry.Watermark) ([]telemetry.Event, telemetry.Watermark, error) {
		cmu.Lock()
		defer cmu.Unlock()
		calls++
		if calls == 1 { // one delta on the first diff tick, then a quiet stream
			return []telemetry.Event{{Serial: "D2AAAA1", Health: "OK", RunningVer: "fw"}}, telemetry.Watermark{Set: true}, nil
		}
		return nil, telemetry.Watermark{Set: true}, nil
	}

	sub, ok := h.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	var mu sync.Mutex
	teleFrames := 0
	go func() {
		for f := range sub.ch {
			if f.name == "telemetry" {
				mu.Lock()
				teleFrames++
				mu.Unlock()
			}
		}
	}()
	waitFor(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return teleFrames == 1 })
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	got := teleFrames
	mu.Unlock()
	if got != 1 {
		t.Errorf("sparse telemetry stream: got %d telemetry frames, want exactly 1 (no churn)", got)
	}
}

// --- T8: periodic re-auth ends the stream on revocation ---

func TestEventsReAuthEndsStream(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := []devicestore.Row{{Serial: "D2AAAA1", Channel: "stable"}}
	// tick irrelevant; reauth fires at 30ms → revoked key tears the stream down.
	cfg := sseConfig{tick: time.Second, ping: time.Second, reauth: 30 * time.Millisecond, writeWindow: time.Second, maxConn: 8}
	revoked := func(context.Context, string) (operator.AuthResult, bool, error) {
		return operator.AuthResult{}, false, nil // key gone / disabled → SEC-M1
	}
	h := newTestHub(life, rows, revoked, cfg)
	eh := &eventsHandler{hub: h}

	srv := httptest.NewServer(http.HandlerFunc(eh.handle))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body) // blocks until the handler ends the stream
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "event: snapshot") {
		t.Errorf("initial snapshot missing: %q", s)
	}
	if !strings.Contains(s, "event: error") || !strings.Contains(s, "revoked") {
		t.Errorf("expected terminal error event on revoked key, got: %q", s)
	}
}

// --- T9: whoami golden field name (is_admin, snake_case) ---

// After W8 whoami is Principal-based (design §4.3, shape {kind, is_admin, scopes, label}) so a cookie
// session reports its real identity, not the empty operator the legacy bearer carrier populated. The
// golden contract is unchanged: is_admin snake_case present, ctxd's `admin` absent.
func TestWhoamiFieldsGoldenShape(t *testing.T) {
	f := whoamiFields(adminhttp.Principal{
		Kind: adminhttp.KindSession, ID: "7", IsAdmin: false, Label: "ro",
		Scopes: []string{adminhttp.ScopeImageRead, adminhttp.ScopeImageWrite},
	})
	if _, ok := f["is_admin"]; !ok {
		t.Error("whoami must expose is_admin (snake_case) — the SPA read-only badge depends on it (D19.6)")
	}
	if _, ok := f["admin"]; ok {
		t.Error("whoami must NOT use ctxd's `admin` field name")
	}
	if f["is_admin"] != false || f["label"] != "ro" || f["kind"] != adminhttp.KindSession {
		t.Errorf("whoami fields mismatch: %+v", f)
	}
}

// TestSSEWriterFlushesThroughIPRateLimit reproduces the exact W4-regression condition the suite missed
// (lead review): main.go wraps the WHOLE mux in adminhttp.IPRateLimit, so the REAL newSSEWriter runs on
// the middleware's status-recording ResponseWriter. Its per-frame ResponseController.Flush() finds the
// Flusher only over the Unwrap() chain — without it the stream silently buffers (newSSEWriter logs and
// degrades, and the fronting proxy 504s the unflushed stream). Red (no statusRecorder.Unwrap): the frame
// flush errors and never reaches the recorder. Green: frames flush through the wrapper.
func TestSSEWriterFlushesThroughIPRateLimit(t *testing.T) {
	adminhttp.ConfigureRateLimitsFromEnv()
	var frameErr error
	h := adminhttp.IPRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		sw := newSSEWriter(w, time.Second)
		frameErr = sw.event("snapshot", "1", []byte(`{"a":1}`))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/events", nil))
	if frameErr != nil {
		t.Fatalf("SSE frame flush through IPRateLimit failed: %v", frameErr)
	}
	if !rec.Flushed {
		t.Fatal("SSE frame never flushed through IPRateLimit's wrapper — statusRecorder must Unwrap()")
	}
	if !strings.Contains(rec.Body.String(), "event: snapshot") {
		t.Fatalf("frame body missing: %q", rec.Body.String())
	}
}

// waitFor polls cond until true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}
