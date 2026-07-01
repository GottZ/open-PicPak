package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// --- unit: no worker, no DB ---

// T16 — clampLimits caps to the production maxima, never raises; a lower request is honored (D25.4). Red:
// a test-run asks for a higher timeout/mem than a prod render and starves the shared worker pool.
func TestClampLimits(t *testing.T) {
	prod := faasproto.Limits{TimeoutMs: 8000, MemMB: 256}
	if got := clampLimits(prod, &faasproto.Limits{TimeoutMs: 999999, MemMB: 8192}); got != prod {
		t.Errorf("above-prod not clamped: %+v", got)
	}
	if got := clampLimits(prod, &faasproto.Limits{TimeoutMs: 2000, MemMB: 64}); got.TimeoutMs != 2000 || got.MemMB != 64 {
		t.Errorf("below-prod not honored: %+v", got)
	}
	if got := clampLimits(prod, nil); got != prod {
		t.Errorf("nil req not prod: %+v", got)
	}
	if got := clampLimits(prod, &faasproto.Limits{}); got != prod {
		t.Errorf("zero req not prod: %+v", got)
	}
	if got := clampLimits(prod, &faasproto.Limits{TimeoutMs: 100000, MemMB: 32}); got.TimeoutMs != 8000 || got.MemMB != 32 {
		t.Errorf("mixed clamp wrong: %+v", got)
	}
}

// T15b — redactSecretValues masks a resolved value out of log[] + err.msg by exact substring, before the
// frame leaves the supervisor (the belt for the deferred real-value mode). Red: a value the function logs
// round-trips to the browser → an admin bearer becomes a store read oracle (Doc 18 D18.3/D18.7 breach).
func TestRedactSecretValues(t *testing.T) {
	meta := &faasproto.ResponseMeta{
		Log: []faasproto.LogLine{{Lvl: "info", Msg: "token is SUPERSECRET here"}},
		Err: &faasproto.RenderErr{Kind: "throw", Msg: "boom with SUPERSECRET"},
	}
	redactSecretValues(meta, map[string]string{"ha_token": "SUPERSECRET"})
	if strings.Contains(meta.Log[0].Msg, "SUPERSECRET") {
		t.Errorf("log leaked value: %q", meta.Log[0].Msg)
	}
	if !strings.Contains(meta.Log[0].Msg, "<redacted:ha_token>") {
		t.Errorf("log not redacted: %q", meta.Log[0].Msg)
	}
	if strings.Contains(meta.Err.Msg, "SUPERSECRET") {
		t.Errorf("err leaked value: %q", meta.Err.Msg)
	}
	// stub default: an empty value map is a no-op → the safe marker stays visible (T15a's premise).
	m2 := &faasproto.ResponseMeta{Log: []faasproto.LogLine{{Msg: "<secret:x> stays"}}}
	redactSecretValues(m2, nil)
	if m2.Log[0].Msg != "<secret:x> stays" {
		t.Errorf("stub marker mangled by empty redact: %q", m2.Log[0].Msg)
	}
}

// --- e2e: the REAL Bun worker (startWorker skips without bun) + optionally the DB ---

func newTestSup(sock string, pool *pgxpool.Pool) *supervisor {
	return &supervisor{
		pool:          pool,
		box:           nil, // stub secrets never open the box
		wake:          WakeConfig{NightStartHour: 23, NightEndHour: 6, DayInterval: 3600, MaxWake: 8 * 3600},
		m4Sock:        sock,
		ditherDefault: "none",
		limits:        faasproto.Limits{TimeoutMs: 8000, MemMB: 256},
		retryWake:     600,
		egress:        nil, // no egress proxy in these tests (no-egress functions)
		cache:         newFrameCache(),
	}
}

func doTestRender(t *testing.T, s *supervisor, jsonBody string) (testMeta, []byte, []byte, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/test-render", strings.NewReader(jsonBody))
	w := httptest.NewRecorder()
	s.handleTestRender(w, r)
	if w.Code != http.StatusOK {
		return testMeta{}, nil, nil, w
	}
	b := w.Body.Bytes()
	if len(b) < 4 {
		t.Fatalf("short frame %d", len(b))
	}
	ml := binary.BigEndian.Uint32(b[:4])
	var m testMeta
	if err := json.Unmarshal(b[4:4+ml], &m); err != nil {
		t.Fatalf("meta decode: %v (%s)", err, string(b[4:4+ml]))
	}
	rest := b[4+ml:]
	if len(rest) < bwry.PackedSize {
		t.Fatalf("body missing packed frame: %d bytes after meta", len(rest))
	}
	return m, rest[:bwry.PackedSize], rest[bwry.PackedSize:], w
}

func solidImageSrc(extra string) string {
	return `export default async (ctx, cap) => { ` + extra +
		` return { image: cap.sharp({create:{width:400,height:300,channels:3,background:{r:0,g:0,b:0}}}) } }`
}

// T15a — a stub-secret test-run injects a MARKER, never a stored value. A function that logs
// cap.secrets.ha_token round-trips "<secret:ha_token>", and the response carries no plaintext (there is
// none). Red: a real value reaches the browser-bound log.
func TestTestRender_StubSecretMarker(t *testing.T) {
	sock := startWorker(t)
	s := newTestSup(sock, nil)
	src := solidImageSrc(`cap.log('info', 'secret=' + cap.secrets.ha_token);`)
	body := `{"source":` + strconv.Quote(src) + `,"secret_bindings":["ha_token"],"serial":"S1","trigger":"render"}`
	meta, packed, raw, w := doTestRender(t, s, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	if !meta.OK || meta.Err != nil {
		t.Fatalf("not ok: %+v", meta.Err)
	}
	if len(packed) != bwry.PackedSize {
		t.Errorf("packed %d != %d", len(packed), bwry.PackedSize)
	}
	if meta.RawFmt != "rgb" || len(raw) != faasproto.RawFrameSize {
		t.Errorf("raw side-by-side missing: fmt=%q len=%d", meta.RawFmt, len(raw))
	}
	joined := ""
	for _, l := range meta.Log {
		joined += l.Msg
	}
	if !strings.Contains(joined, "<secret:ha_token>") {
		t.Errorf("stub marker not in log: %q", joined)
	}
}

// T9 — the trigger + payload route into ctx: a webhook run sees ctx.trigger.payload; a render run sees
// none. (Worker truth: the payload rides ctx.trigger.payload, NOT ctx.payload — the design doc's stated
// shape.) Red: the webhook payload never reaches the function, so the push-to-frame path can't be tested.
func TestTestRender_TriggerPayload(t *testing.T) {
	sock := startWorker(t)
	s := newTestSup(sock, nil)
	src := solidImageSrc(`cap.log('info', 'trig=' + ctx.trigger.type + ' pl=' + JSON.stringify(ctx.trigger.payload));`)

	meta, _, _, w := doTestRender(t, s,
		`{"source":`+strconv.Quote(src)+`,"serial":"S1","trigger":"webhook","payload":{"k":"v"}}`)
	if w.Code != http.StatusOK || !meta.OK {
		t.Fatalf("webhook run failed: code=%d %+v", w.Code, meta.Err)
	}
	joined := ""
	for _, l := range meta.Log {
		joined += l.Msg
	}
	if !strings.Contains(joined, `trig=webhook`) || !strings.Contains(joined, `pl={"k":"v"}`) {
		t.Errorf("webhook payload not routed to ctx.trigger.payload: %q", joined)
	}

	meta2, _, _, _ := doTestRender(t, s, `{"source":`+strconv.Quote(src)+`,"serial":"S1","trigger":"render"}`)
	joined2 := ""
	for _, l := range meta2.Log {
		joined2 += l.Msg
	}
	if !strings.Contains(joined2, "trig=render") || strings.Contains(joined2, `"k":"v"`) {
		t.Errorf("render run leaked a payload: %q", joined2)
	}
}

// T5 — THE poison test (D25.4): a test-run against a device-bound saved function leaves that device's
// production cache + last-good BYTE-IDENTICAL. Red: the test path writes last-good → the next real device
// poll serves the TEST frame (fleet poisoned by a preview).
func TestTestRender_SideEffectFree(t *testing.T) {
	sock := startWorker(t)
	pool := faasTestDBPool(t)
	ctx := context.Background()
	s := newTestSup(sock, pool)

	mustExecSup(t, pool, `INSERT INTO devices (serial, channel) VALUES ('S1','stable')`)
	id, err := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "poison-fn", Source: solidImageSrc(``), TriggerType: faasstore.TriggerRender,
	})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, "S1", id); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// prior fleet state: a KNOWN last-good + a distinct in-memory cache frame.
	prior := bytes.Repeat([]byte{0xAB}, bwry.PackedSize)
	priorCache := bytes.Repeat([]byte{0xCD}, bwry.PackedSize)
	if err := faasstore.LastGoodPut(ctx, pool, "S1", id, prior, "ok"); err != nil {
		t.Fatalf("seed last-good: %v", err)
	}
	s.cache.put("S1", id, 1, priorCache)

	meta, _, _, w := doTestRender(t, s, `{"id":`+strconv.FormatInt(id, 10)+`,"serial":"S1","trigger":"render"}`)
	if w.Code != http.StatusOK || !meta.OK {
		t.Fatalf("saved-fn test-run failed: code=%d %+v", w.Code, meta.Err)
	}

	lg, err := faasstore.LastGoodGet(ctx, pool, "S1", id)
	if err != nil || lg == nil {
		t.Fatalf("last-good gone: %v", err)
	}
	if !bytes.Equal(lg.Packed, prior) {
		t.Error("test-run OVERWROTE last-good — the fleet would be poisoned by a preview")
	}
	if e, ok := s.cache.get("S1", id); !ok || !bytes.Equal(e.packed, priorCache) {
		t.Error("test-run mutated the production hot-cache")
	}
}

func faasTestDBPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbPool(t) // skips without TEST_DATABASE_URL; truncates secrets
	for _, stmt := range []string{`DELETE FROM faas_functions`, `DELETE FROM devices`} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	return pool
}

func mustExecSup(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
