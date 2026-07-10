package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/open-picpak/backend/internal/bwry"
	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// A31.2 — per-function dither reaches the PRODUCTION pack. Before this wave doRender packed with the
// undithered bwry.Pack regardless of trigger_config.dither, so the operator's per-function selector was
// silently dropped on the served fleet. These probes drive the real production render primitive
// (s.doRender → renderOnce → bwry.PackWithDither) over a fake M4 worker; they are DB-free (no secret
// bindings, no egress) so they run without TEST_DATABASE_URL. The buildframe_test renderFunc stub
// bypasses renderOnce's pack entirely and therefore CANNOT exercise the resolution under test.

// gradientRGB builds a 400x300 raw RGB frame whose columns ramp R+G 0→255 over a constant mid-tone
// blue. It is deliberately NON-palette (a solid palette colour would pack identically under none and
// atkinson, making the probe vacuous): nearest-quantize and Atkinson error-diffusion produce DIFFERENT
// packed bytes on it, which the guard below asserts.
func gradientRGB() []byte {
	pix := make([]byte, bwry.Width*bwry.Height*3)
	for y := 0; y < bwry.Height; y++ {
		for x := 0; x < bwry.Width; x++ {
			o := (y*bwry.Width + x) * 3
			v := byte(x * 255 / (bwry.Width - 1))
			pix[o] = v     // R ramp
			pix[o+1] = v   // G ramp
			pix[o+2] = 128 // constant non-palette blue → residual diffusion error
		}
	}
	return pix
}

// fakeWorker starts a one-shot M4 worker on a fresh UDS: it reads one render-request frame and replies
// with a render-response carrying the given meta.Dither echo + raw RGB (faasproto.EncodeResponse). This
// lets a probe drive renderOnce's REAL pack path without a Bun worker, and lets it plant an UNTRUSTED
// worker-return dither for the precedence probe.
func fakeWorker(t *testing.T, workerReturn string, raw []byte) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "m4.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	meta := faasproto.ResponseMeta{V: 1, OK: true, W: bwry.Width, H: bwry.Height, Dither: workerReturn}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := faasproto.ReadFrame(conn); err != nil {
			return
		}
		payload, err := faasproto.EncodeResponse(meta, raw)
		if err != nil {
			return
		}
		_ = faasproto.WriteFrame(conn, faasproto.KindRenderResponse, payload)
	}()
	return sock
}

// ditherProbeSup builds the minimal supervisor doRender needs: a fake worker socket, DITHER_DEFAULT
// "none", and a non-nil egress client (never called — the probe function binds no egress). pool/box stay
// nil because the probe function binds no secrets, so resolveSecrets never touches them.
func ditherProbeSup(sock string) *supervisor {
	return &supervisor{
		ditherDefault: "none",
		limits:        faasproto.Limits{TimeoutMs: 8000, MemMB: 256},
		m4Sock:        sock,
		egress:        newEgressClient(""),
	}
}

// renderWithConfig drives the PRODUCTION render primitive for a function whose trigger_config is the
// given JSON (or none), with the worker echoing workerReturn as its untrusted dither.
func renderWithConfig(t *testing.T, triggerConfig, workerReturn string) []byte {
	t.Helper()
	sock := fakeWorker(t, workerReturn, gradientRGB())
	s := ditherProbeSup(sock)
	fn := &faasstore.Function{ID: 1, Version: 1}
	if triggerConfig != "" {
		fn.TriggerConfig = json.RawMessage(triggerConfig)
	}
	res := s.doRender(context.Background(), fn, renderCtx(), true)
	if res.Err != nil {
		t.Fatalf("doRender err: %+v", res.Err)
	}
	if len(res.Packed) != bwry.PackedSize {
		t.Fatalf("packed %d != %d", len(res.Packed), bwry.PackedSize)
	}
	return res.Packed
}

// wantNone / wantAtkinson are the reference frames for gradientRGB, computed from the SAME bwry pack
// functions the golden tests pin. They are asserted DISTINCT so every probe below is non-vacuous.
func ditherRefs(t *testing.T) (none, atkinson []byte) {
	t.Helper()
	img, err := bwry.FromRGB(gradientRGB())
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}
	if none, err = bwry.PackWithDither(img, bwry.DitherNone); err != nil {
		t.Fatalf("pack none: %v", err)
	}
	if atkinson, err = bwry.PackWithDither(img, bwry.DitherAtkinson); err != nil {
		t.Fatalf("pack atkinson: %v", err)
	}
	if bytes.Equal(none, atkinson) {
		t.Fatalf("gradient does not distinguish none from atkinson — probe would be vacuous")
	}
	return none, atkinson
}

// Probe (a): trigger_config.dither=atkinson → the production pack must emit Atkinson bytes, NOT none.
// RED before A31.2 (doRender packed undithered bwry.Pack ⇒ none bytes); GREEN after (trigger_config
// wired into RenderOpts.Dither → resolveDitherStr → PackWithDither(atkinson)).
func TestPerFnDither_ConfigAtkinson(t *testing.T) {
	none, atkinson := ditherRefs(t)
	got := renderWithConfig(t, `{"dither":"atkinson"}`, "")
	if bytes.Equal(got, none) {
		t.Fatal("trigger_config.dither=atkinson still packed as none — production ignores per-function dither (A31.2 gap)")
	}
	if !bytes.Equal(got, atkinson) {
		t.Fatal("trigger_config.dither=atkinson did not produce Atkinson bytes")
	}
}

// Probe (b): a function with NO trigger_config.dither packs none bytes — byte-identical to today
// (A31-E2 back-compat, no data rewrite).
func TestPerFnDither_NoConfigBackCompat(t *testing.T) {
	none, _ := ditherRefs(t)
	got := renderWithConfig(t, "", "")
	if !bytes.Equal(got, none) {
		t.Fatal("function without dither diverged from none — A31-E2 back-compat regression")
	}
}

// Probe (c) — PRECEDENCE: trigger_config.dither="none" (trusted) AND the worker return dither="atkinson"
// (untrusted). The trusted config must WIN → none bytes. RED if the untrusted return overrode config
// (Policy=Data breach: untrusted worker JS rewriting trusted operator policy); GREEN with the
// trusted-first order in resolveDitherStr.
func TestPerFnDither_TrustedConfigBeatsWorkerReturn(t *testing.T) {
	none, atkinson := ditherRefs(t)
	got := renderWithConfig(t, `{"dither":"none"}`, "atkinson")
	if bytes.Equal(got, atkinson) {
		t.Fatal("untrusted worker return overrode trusted trigger_config — Policy=Data breach")
	}
	if !bytes.Equal(got, none) {
		t.Fatal("trusted trigger_config.dither=none was not honoured")
	}
}

// Untrusted worker return IS honoured as a fallback when no trusted policy is set (safe: ParseDither is
// fail-closed and the selector space is deterministic + secret-free). Confirms the fallback tier exists
// and is not dead.
func TestPerFnDither_WorkerReturnFallback(t *testing.T) {
	_, atkinson := ditherRefs(t)
	got := renderWithConfig(t, "", "atkinson")
	if !bytes.Equal(got, atkinson) {
		t.Fatal("worker return atkinson was not honoured as the fallback tier")
	}
}

// resolveDitherStr precedence + fail-closed table (pure, no worker).
func TestResolveDitherStr(t *testing.T) {
	cases := []struct {
		name, trigger, ret, def, want string
	}{
		{"trusted wins over return", "none", "atkinson", "none", "none"},
		{"trusted wins over return 2", "atkinson", "none", "none", "atkinson"},
		{"return fallback when no trigger", "", "atkinson", "none", "atkinson"},
		{"default when neither", "", "", "atkinson", "atkinson"},
		{"none when all empty (back-compat)", "", "", "", "none"},
		{"unparsable trigger falls through to return", "bogus", "atkinson", "none", "atkinson"},
		{"unparsable trigger+return falls to default", "bogus", "typo", "none", "none"},
		{"all unparsable → none (fail-closed)", "BOGUS", "Atkinson", "floyd-steinberg", "none"},
	}
	for _, tc := range cases {
		if got := resolveDitherStr(tc.trigger, tc.ret, tc.def); got != tc.want {
			t.Errorf("%s: resolveDitherStr(%q,%q,%q)=%q want %q", tc.name, tc.trigger, tc.ret, tc.def, got, tc.want)
		}
	}
}
