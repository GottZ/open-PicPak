package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// faasPool opens the ephemeral DB (0009 applied) and truncates the FaaS tables for a clean slate.
func faasPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — build_frame DB tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE faas_functions, device_render_binding, faas_frame_lastgood RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func testSupervisor(pool *pgxpool.Pool, render renderFunc) *supervisor {
	return &supervisor{
		pool:      pool,
		cache:     newFrameCache(),
		wake:      WakeConfig{NightStartHour: 23, NightEndHour: 6, DayInterval: 3600, MaxWake: 8 * 3600},
		renderTTL: 120 * time.Second,
		retryWake: 600,
		render:    render,
		lastRun:   map[int64]time.Time{},
	}
}

func frameOf(b byte) []byte { return bytes.Repeat([]byte{b}, 30000) }

func okRender(packed []byte) renderFunc {
	return func(_ context.Context, _ *faasstore.Function, _ faasproto.RequestCtx, _ bool) RenderResult {
		return RenderResult{Packed: packed, Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
}

func failRender() renderFunc {
	return func(_ context.Context, _ *faasstore.Function, _ faasproto.RequestCtx, _ bool) RenderResult {
		re := &faasproto.RenderErr{Kind: "throw", Msg: "boom"}
		return RenderResult{Meta: faasproto.ResponseMeta{V: 1, OK: false, Err: re}, Err: re}
	}
}

func mkSyncFn(t *testing.T, pool *pgxpool.Pool) *faasstore.Function {
	t.Helper()
	id, err := faasstore.Create(context.Background(), pool, faasstore.CreateParams{
		Name: "sync-fn", Source: "x", TriggerType: faasstore.TriggerRender,
		TriggerConfig: json.RawMessage(`{"mode":"sync","ttl_s":120}`),
	})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	fn, err := faasstore.LoadFunction(context.Background(), pool, id)
	if err != nil {
		t.Fatalf("load fn: %v", err)
	}
	return fn
}

// T6 — cache parity: two sync requests within TTL drive the worker ONCE; force + a version bump each
// re-render (K7 cache-bust).
func TestBuildFrameCacheParity(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	fn := mkSyncFn(t, pool)
	frame := frameOf(0x11)
	calls := 0
	render := func(c context.Context, f *faasstore.Function, r faasproto.RequestCtx, force bool) RenderResult {
		calls++
		return RenderResult{Packed: frame, Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
	s := testSupervisor(pool, render)
	rctx := faasproto.RequestCtx{Serial: "S1"}

	p1, st1, _, _ := s.buildFrame(ctx, fn, rctx, false)
	p2, st2, _, _ := s.buildFrame(ctx, fn, rctx, false) // within TTL → cache hit
	if calls != 1 {
		t.Fatalf("worker driven %d times within TTL, want 1 (T6 cache parity)", calls)
	}
	if st1 != "ok" || st2 != "ok" || !bytes.Equal(p1, frame) || !bytes.Equal(p2, frame) {
		t.Fatalf("cache: st=(%s,%s) frames match=%v", st1, st2, bytes.Equal(p1, p2))
	}

	s.buildFrame(ctx, fn, rctx, true) // force bypasses the cache
	if calls != 2 {
		t.Fatalf("force should re-render, calls=%d", calls)
	}

	// version bump (source edit) → cache miss → re-render (K7).
	if _, err := faasstore.Update(ctx, pool, fn.ID, faasstore.UpdateParams{Source: "y"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	fn2, _ := faasstore.LoadFunction(ctx, pool, fn.ID)
	s.buildFrame(ctx, fn2, rctx, false)
	if calls != 3 {
		t.Fatalf("version bump should re-render (K7 cache-bust), calls=%d", calls)
	}
}

// T7 — 3-stage fallback: warm cache → durable last-good → embedded error frame.
func TestBuildFrame3StageFallback(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	fn := mkSyncFn(t, pool)
	good := frameOf(0x22)

	// prime a successful render (caches + persists last-good for S1).
	s := testSupervisor(pool, okRender(good))
	s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, false)

	// stage 1 — warm in-memory cache: force a re-render that fails → serve the warm frame stale.
	s.render = failRender()
	p, st, stale, wake := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, true)
	if st != "stale" || !stale || !bytes.Equal(p, good) || wake != 600 {
		t.Fatalf("stage1 warm cache: st=%s stale=%v match=%v wake=%d", st, stale, bytes.Equal(p, good), wake)
	}

	// stage 2 — cold cache (fresh supervisor), durable last-good present, render fails → DB stale.
	cold := testSupervisor(pool, failRender())
	p, st, stale, _ = cold.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, false)
	if st != "stale" || !stale || !bytes.Equal(p, good) {
		t.Fatalf("stage2 DB last-good: st=%s stale=%v match=%v", st, stale, bytes.Equal(p, good))
	}

	// stage 3 — cold cache, no last-good (unseen serial), render fails → embedded error frame.
	p, st, stale, _ = cold.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S-UNSEEN"}, false)
	if st != "error" || !stale || !bytes.Equal(p, errorFrame) {
		t.Fatalf("stage3 error frame: st=%s stale=%v match=%v", st, stale, bytes.Equal(p, errorFrame))
	}
}

// T11 (fan-out) — a schedule/prerender function renders EVERY bound serial with its own ctx.serial,
// writing per-(serial, fn) last-good. Device B never gets device A's frame.
func TestFanOut(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	id, err := faasstore.Create(ctx, pool, faasstore.CreateParams{Name: "sched", Source: "x", TriggerType: faasstore.TriggerSchedule})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, "SN-A", id); err != nil {
		t.Fatalf("bind A: %v", err)
	}
	if err := faasstore.BindDevice(ctx, pool, "SN-B", id); err != nil {
		t.Fatalf("bind B: %v", err)
	}
	perSerial := func(_ context.Context, _ *faasstore.Function, r faasproto.RequestCtx, _ bool) RenderResult {
		var b byte
		switch r.Serial {
		case "SN-A":
			b = 0xAA
		case "SN-B":
			b = 0xBB
		}
		return RenderResult{Packed: frameOf(b), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
	s := testSupervisor(pool, perSerial)
	fn, _ := faasstore.LoadFunction(ctx, pool, id)

	ok, failed := s.fanOut(ctx, fn, faasproto.Trigger{Type: "schedule"})
	if ok != 2 || failed != 0 {
		t.Fatalf("fanOut: ok=%d failed=%d, want 2/0", ok, failed)
	}
	lgA, _ := faasstore.LastGoodGet(ctx, pool, "SN-A", id)
	lgB, _ := faasstore.LastGoodGet(ctx, pool, "SN-B", id)
	if lgA == nil || lgB == nil || lgA.Packed[0] != 0xAA || lgB.Packed[0] != 0xBB {
		t.Fatalf("fan-out wrote the wrong per-serial frames: A=%v B=%v", lgA, lgB)
	}
}

// T11 (routing) — render/sync renders inline; every other trigger serves last-good only.
func TestTriggerRouting(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	s := testSupervisor(pool, failRender())

	sync := &faasstore.Function{TriggerType: faasstore.TriggerRender, TriggerConfig: json.RawMessage(`{"mode":"sync"}`)}
	pre := &faasstore.Function{TriggerType: faasstore.TriggerRender, TriggerConfig: json.RawMessage(`{"mode":"prerender"}`)}
	sched := &faasstore.Function{TriggerType: faasstore.TriggerSchedule}
	hook := &faasstore.Function{TriggerType: faasstore.TriggerWebhook}
	if !s.syncInline(sync) {
		t.Error("render/sync should render inline")
	}
	for _, fn := range []*faasstore.Function{pre, sched, hook} {
		if s.syncInline(fn) {
			t.Errorf("%s/%s should serve last-good, not render inline", fn.TriggerType, parseTriggerConfig(fn.TriggerConfig).Mode)
		}
	}

	// serveLastGood on a serial with no last-good → error frame (nothing rendered yet).
	fnID, err := faasstore.Create(ctx, pool, faasstore.CreateParams{Name: "pre", Source: "x", TriggerType: faasstore.TriggerRender, TriggerConfig: json.RawMessage(`{"mode":"prerender"}`)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fn, _ := faasstore.LoadFunction(ctx, pool, fnID)
	p, st, _, _ := s.serveLastGood(ctx, fn, faasproto.RequestCtx{Serial: "S1"})
	if st != "error" || !bytes.Equal(p, errorFrame) {
		t.Fatalf("serveLastGood with no frame: st=%s, want error frame", st)
	}
	// after a fan-out writes last-good, serveLastGood returns it.
	if err := faasstore.LastGoodPut(ctx, pool, "S1", fnID, frameOf(0x33), "ok"); err != nil {
		t.Fatalf("put: %v", err)
	}
	p, st, stale, _ := s.serveLastGood(ctx, fn, faasproto.RequestCtx{Serial: "S1"})
	if st != "ok" || stale || p[0] != 0x33 {
		t.Fatalf("serveLastGood after write: st=%s stale=%v", st, stale)
	}
}
