package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/templatestore"
)

// multiWorker is a MULTI-connection fake M4 worker: it loops accepting and answers EVERY drive with a
// fixed raw RGB frame (dither_test's fakeWorker is one-shot). The e2e probe needs it so the RED path —
// a non-invariant catalog where the second serial DOES drive the worker — lands on the dither/drive-count
// assertions instead of hanging on an unserved second connection. *drives counts real worker exchanges.
func multiWorker(t *testing.T, raw []byte, drives *atomic.Int64) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "m4.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	meta := faasproto.ResponseMeta{V: 1, OK: true, W: bwry.Width, H: bwry.Height}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, _, err := faasproto.ReadFrame(c); err != nil {
					return
				}
				drives.Add(1)
				payload, err := faasproto.EncodeResponse(meta, raw)
				if err != nil {
					return
				}
				_ = faasproto.WriteFrame(c, faasproto.KindRenderResponse, payload)
			}(conn)
		}
	}()
	return sock
}

// A31.6 — the flagship external-image builtin, driven END-TO-END FROM THE SEEDED CATALOG through the
// real render path. This is the wave's integration proof: it does NOT hardcode the dither or the
// serial_invariant flag. It seeds builtins/catalog.json into `templates` (templatestore.SeedBuiltins),
// reads builtin/external-image's trigger_config straight out of the DB, hangs it on a faasstore
// function (as the substitute path A30-W6 would), and drives buildFrame over a fake M4 worker returning
// a non-palette gradient. Both assertions ride the ONE catalog value:
//
//   - dither: the frame packs ATKINSON bytes (not none) because trigger_config.dither=atkinson reaches
//     the trusted tier of renderOnce's pack (A31.2).
//   - serial_invariant: TWO different serials drive the worker exactly ONCE and get identical bytes,
//     because trigger_config.serial_invariant=true routes buildFrame through the (fn,version) invariant
//     cache (A31.5).
//
// RED against the A30-W1 placeholder catalog ({"mode":"sync","ttl_s":900}: no dither ⇒ none bytes; no
// serial_invariant ⇒ two drives). GREEN after the A31.6 catalog completion. DB-backed (buildFrame writes
// last-good + reads the invariant cache) ⇒ needs TEST_DATABASE_URL.
func TestExternalImage_SeededCatalogDrivesAtkinsonAndSingleDrive(t *testing.T) {
	pool := faasPool(t)
	ctx := context.Background()

	// Seed the embedded catalog into `templates`, then read the external-image trigger_config the seed
	// actually installed — the value under test comes from the catalog, never from this test body.
	if _, err := templatestore.SeedBuiltins(ctx, pool); err != nil {
		t.Fatalf("seed builtins: %v", err)
	}
	var triggerCfg, source string
	if err := pool.QueryRow(ctx,
		`SELECT trigger_config::text, source FROM templates WHERE name = 'builtin/external-image'`).
		Scan(&triggerCfg, &source); err != nil {
		t.Fatalf("read seeded external-image: %v", err)
	}

	// Instantiate the builtin as an operator apply would, carrying the SEEDED trigger_config verbatim.
	// egress_allow is intentionally left empty: the fake worker performs no fetch, and the egress wall is
	// the network-layer proxy — not exercisable in this unit harness (documented gap).
	fnID, err := faasstore.Create(ctx, pool, faasstore.CreateParams{
		Name: "ext-img-e2e", Source: source, TriggerType: faasstore.TriggerRender,
		TriggerConfig: json.RawMessage(triggerCfg),
	})
	if err != nil {
		t.Fatalf("create fn: %v", err)
	}
	fn, err := faasstore.LoadFunction(ctx, pool, fnID)
	if err != nil {
		t.Fatalf("load fn: %v", err)
	}

	// Drive the REAL production render primitive (s.doRender → renderOnce → bwry.PackWithDither) over a
	// multi-connection fake M4 worker returning a non-palette gradient, counting real worker exchanges so
	// the serial_invariant single-drive gate is observable end-to-end from the catalog value.
	var drives atomic.Int64
	sock := multiWorker(t, gradientRGB(), &drives)
	s := &supervisor{
		pool:          pool,
		cache:         newFrameCache(),
		wake:          WakeConfig{NightStartHour: 23, NightEndHour: 6, DayInterval: 3600, MaxWake: 8 * 3600},
		renderTTL:     120 * time.Second,
		retryWake:     600,
		ditherDefault: "none",
		limits:        faasproto.Limits{TimeoutMs: 8000, MemMB: 256},
		m4Sock:        sock,
		egress:        newEgressClient(""), // nil client — register/unregister are nil-safe no-ops
		lastRun:       map[int64]time.Time{},
	}
	s.render = s.doRender

	// Reference frames from the SAME bwry pack the golden tests pin, asserted DISTINCT (non-vacuous).
	none, atkinson := ditherRefs(t)

	p1, st1, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "SN-1"}, false)
	p2, st2, _, _ := s.buildFrame(ctx, fn, faasproto.RequestCtx{Serial: "SN-2"}, false)

	if st1 != "ok" || st2 != "ok" {
		t.Fatalf("status not ok: %s %s", st1, st2)
	}
	// dither, from the catalog: Atkinson bytes, not none.
	if bytes.Equal(p1, none) {
		t.Fatal("seeded external-image packed as none — catalog trigger_config.dither not reaching the pack (placeholder catalog / A31.6 gap)")
	}
	if !bytes.Equal(p1, atkinson) {
		t.Fatal("seeded external-image did not pack Atkinson bytes")
	}
	// serial_invariant, from the catalog: one drive for two serials, identical bytes.
	if n := drives.Load(); n != 1 {
		t.Fatalf("two serials drove the worker %d times, want 1 (catalog serial_invariant not effective end-to-end)", n)
	}
	if !bytes.Equal(p1, p2) {
		t.Fatalf("serials got DIFFERENT frames (%#x vs %#x) — invariant cache not shared", p1[0], p2[0])
	}
}
