package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// A31.5 — serial-invariant render cache. When trigger_config.serial_invariant is true the frame is
// PROVABLY identical for every bound serial, so the whole fleet must share ONE render per (fn, version,
// TTL) instead of one render per serial. These probes drive the injectable renderFunc with a
// serial-dependent stub + a drive counter: RED against the per-serial key (each serial drives the
// worker and gets DIFFERENT bytes), GREEN after the invariant branch (first serial drives, the rest
// reuse those exact bytes). DB-backed (per-serial last-good writes) — needs TEST_DATABASE_URL.

// mkInvariantFn creates a render/sync function whose trigger_config sets serial_invariant to the given
// value (absent when false → exact back-compat).
func mkInvariantFn(t *testing.T, pool *pgxpool.Pool, name string, invariant bool) *faasstore.Function {
	t.Helper()
	cfg := `{"mode":"sync","ttl_s":120}`
	if invariant {
		cfg = `{"mode":"sync","ttl_s":120,"serial_invariant":true}`
	}
	id, err := faasstore.Create(context.Background(), pool, faasstore.CreateParams{
		Name: name, Source: "x", TriggerType: faasstore.TriggerRender,
		TriggerConfig: json.RawMessage(cfg),
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

// serialByteRender returns a render stub that packs a serial-dependent frame and bumps *calls on every
// real drive. Two DIFFERENT serials yield DIFFERENT bytes on a drive, so a shared frame across serials
// can only come from the invariant cache (no second drive), never from the stub.
func serialByteRender(calls *int, byteFor map[string]byte) renderFunc {
	return func(_ context.Context, _ *faasstore.Function, r faasproto.RequestCtx, _ bool) RenderResult {
		*calls++
		return RenderResult{Packed: frameOf(byteFor[r.Serial]), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
	}
}

// Probe (a) buildFrame — two DIFFERENT serials against the same serial_invariant:true fn drive the
// worker exactly ONCE and both receive identical bytes. RED against the per-serial key (S1→0xA1,
// S2→0xB2, two drives); GREEN after the (fn.ID, version)-keyed branch (S1 drives, S2 reuses S1's bytes).
func TestSerialInvariant_BuildFrameSingleDrive(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	fn := mkInvariantFn(t, pool, "inv-build", true)
	calls := 0
	s := testSupervisor(pool, serialByteRender(&calls, map[string]byte{"S1": 0xA1, "S2": 0xB2}))

	p1, st1, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, false)
	p2, st2, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S2"}, false)

	if calls != 1 {
		t.Fatalf("serial_invariant fn drove the worker %d times for 2 serials, want 1 (A31.5)", calls)
	}
	if !bytes.Equal(p1, p2) {
		t.Fatalf("serial_invariant serials got DIFFERENT frames (%#x vs %#x) — invariant cache not shared", p1[0], p2[0])
	}
	if p1[0] != 0xA1 {
		t.Fatalf("invariant frame is not the first renderer's bytes: %#x", p1[0])
	}
	if st1 != "ok" || st2 != "ok" {
		t.Fatalf("status not ok: %s %s", st1, st2)
	}
}

// Probe (a) fanOut — the fan-out writer drives the worker ONCE for a serial_invariant schedule fn and
// writes identical per-serial last-good for every bound serial. RED against the per-serial key (SN-A and
// SN-B each drive → different last-good); GREEN after the branch (SN-A drives, SN-B reuses).
func TestSerialInvariant_FanOutSingleDrive(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	id, err := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "inv-sched", Source: "x", TriggerType: faasstore.TriggerSchedule,
		TriggerConfig: json.RawMessage(`{"serial_invariant":true}`),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, sn := range []string{"SN-A", "SN-B"} {
		if err := faasstore.BindDevice(ctx, pool, sn, id); err != nil {
			t.Fatalf("bind %s: %v", sn, err)
		}
	}
	fn, _ := faasstore.LoadFunction(ctx, pool, id)
	calls := 0
	s := testSupervisor(pool, serialByteRender(&calls, map[string]byte{"SN-A": 0xAA, "SN-B": 0xBB}))

	ok, failed := s.fanOut(ctx, fn, faasproto.Trigger{Type: "schedule"})
	if ok != 2 || failed != 0 {
		t.Fatalf("fanOut ok=%d failed=%d, want 2/0", ok, failed)
	}
	if calls != 1 {
		t.Fatalf("serial_invariant fanOut drove the worker %d times for 2 serials, want 1 (A31.5)", calls)
	}
	lgA, _ := faasstore.LastGoodGet(ctx, pool, "SN-A", id)
	lgB, _ := faasstore.LastGoodGet(ctx, pool, "SN-B", id)
	if lgA == nil || lgB == nil {
		t.Fatalf("missing last-good: A=%v B=%v", lgA, lgB)
	}
	if !bytes.Equal(lgA.Packed, lgB.Packed) {
		t.Fatalf("serial_invariant fanOut wrote DIFFERENT per-serial frames (%#x vs %#x)", lgA.Packed[0], lgB.Packed[0])
	}
	if lgA.Packed[0] != 0xAA {
		t.Fatalf("invariant fanOut frame is not the first renderer's bytes: %#x", lgA.Packed[0])
	}
}

// Probe (b) back-compat — a fn WITHOUT serial_invariant keeps the per-serial key: two serials drive the
// worker twice and get their own distinct frames. GREEN throughout (asserts the absence of the branch
// for non-invariant fns), logged in the RED run as the back-compat guard.
func TestSerialInvariant_BackCompatPerSerial(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	fn := mkInvariantFn(t, pool, "plain-build", false)
	calls := 0
	s := testSupervisor(pool, serialByteRender(&calls, map[string]byte{"S1": 0xA1, "S2": 0xB2}))

	p1, _, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, false)
	p2, _, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S2"}, false)

	if calls != 2 {
		t.Fatalf("non-invariant fn drove the worker %d times for 2 serials, want 2 (back-compat)", calls)
	}
	if bytes.Equal(p1, p2) {
		t.Fatalf("non-invariant serials shared a frame — per-serial isolation regressed")
	}
	if p1[0] != 0xA1 || p2[0] != 0xB2 {
		t.Fatalf("non-invariant serials got the wrong frames: %#x %#x", p1[0], p2[0])
	}
}

// Probe (c) version bump — an invariant fn warms the invariant cache, then a version bump forces a fresh
// drive (the (fn.ID, version) key changes → miss), never serving a stale frame across the bump (K7).
func TestSerialInvariant_VersionBump(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()
	fn := mkInvariantFn(t, pool, "inv-ver", true)
	calls := 0
	s := testSupervisor(pool, serialByteRender(&calls, map[string]byte{"S1": 0xA1, "S2": 0xA1, "S3": 0xA1}))

	s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S1"}, false) // drive 1 → warms invariant cache
	s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "S2"}, false) // invariant hit → no drive
	if calls != 1 {
		t.Fatalf("warm invariant cache drove the worker %d times, want 1", calls)
	}

	if _, err := faasstore.Update(ctx, pool, fn.ID, faasstore.UpdateParams{Source: "y"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	fn2, _ := faasstore.LoadFunction(ctx, pool, fn.ID)
	s.buildFrame(ctx, fn2, faasproto.RequestCtx{Serial: "S3"}, false) // version bumped → invariant miss → drive
	if calls != 2 {
		t.Fatalf("version bump did not re-drive the worker (stale serve across the bump), calls=%d", calls)
	}
}
