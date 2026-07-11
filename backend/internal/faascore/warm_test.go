package faascore

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/plrender"
)

// warmTracker is a playlistRenderFunc that records peak concurrency, so the staffel gate can assert
// the warmer never exceeds its parallelism limit. It holds each render briefly so concurrent warms
// overlap (without the hold every render would finish before the next starts and peak would be 1).
type warmTracker struct {
	mu          sync.Mutex
	calls       int
	inflight    int
	maxInflight int
}

func (w *warmTracker) render(_ context.Context, _ string, _ []byte, _ string) RenderResult {
	w.mu.Lock()
	w.calls++
	w.inflight++
	if w.inflight > w.maxInflight {
		w.maxInflight = w.inflight
	}
	w.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	w.mu.Lock()
	w.inflight--
	w.mu.Unlock()
	return RenderResult{Packed: bytes.Repeat([]byte{0x11}, 30000), Meta: faasproto.ResponseMeta{V: 1, OK: true}}
}

// TestWarmPlaylistStaggered is the storm gate (K5): warming an N-variant playlist drives EVERY variant
// exactly once but never runs more than warmLimit renders at a time (red without the semaphore: peak =
// N, the inline sync-render storm K5 forbids).
func TestWarmPlaylistStaggered(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	wt := &warmTracker{}
	s.renderPlaylist = wt.render
	s.warmLimit = 2
	ctx := context.Background()

	const n = 6
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = h.seedImage(t, i+1, byte(0xA0+i)) // distinct bytes → distinct sha → distinct variant
	}
	plID := h.seedPlaylist(t, "sequential", ids...)

	s.warmPlaylist(ctx, plID)

	if wt.calls != n {
		t.Fatalf("warmer drove %d renders, want %d (every distinct variant packed once)", wt.calls, n)
	}
	if wt.maxInflight > s.warmLimit {
		t.Fatalf("warmer peaked at %d concurrent renders, want <= %d (K5 staffel)", wt.maxInflight, s.warmLimit)
	}
	if wt.maxInflight < 2 {
		t.Fatalf("warmer peaked at %d, want it to use the limit (>=2) — otherwise the stagger bound is untested", wt.maxInflight)
	}
}

// TestWarmDedupsVariants proves the warmer packs a repeated (image,fit,dither) variant only ONCE: two
// items sharing the same image+policy are one fleet-shared variant, so one worker drive covers both.
func TestWarmDedupsVariants(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	s.warmLimit = 2
	ctx := context.Background()
	id := h.seedImage(t, 1, 0x42)
	plID := h.seedPlaylist(t, "sequential", id, id, id) // same image three times → one variant

	s.warmPlaylist(ctx, plID)

	if h.callCount() != 1 {
		t.Fatalf("warmer drove %d renders for one distinct variant, want 1 (content-addressed dedup)", h.callCount())
	}
}

// TestWarmThenRenderIsCacheHit is the production-path gate: a pre-warmed variant is served to a live
// /frame from the fleet-shared cache with NO second worker drive — proving the warmer packed through
// the real VariantPut path (s.packedVariant), not a side channel. Red if the warmer bypassed the
// cache: the device render would drive the worker again (callCount 2).
func TestWarmThenRenderIsCacheHit(t *testing.T) {
	h := newPlHarness(t)
	s := h.supervisor()
	s.warmLimit = 2
	ctx := context.Background()
	id := h.seedImage(t, 1, 0x42)
	plID := h.seedPlaylist(t, "sequential", id)
	if err := plrender.BindPlaylist(ctx, h.pool, "DEV", plID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	s.warmPlaylist(ctx, plID)
	if h.callCount() != 1 {
		t.Fatalf("warm drove %d renders, want 1", h.callCount())
	}

	packed, status, _, _ := s.buildPlaylistFrame(ctx, "DEV", plID, time.Now())
	if h.callCount() != 1 {
		t.Fatalf("render after warm drove the worker again (total %d) — warmer did not use the production cache path", h.callCount())
	}
	if status != "ok" || packed[0] != 0x42 {
		t.Fatalf("served frame status=%s marker=%#x, want ok/0x42", status, packed[0])
	}
}

// TestPlaylistPushDisabledByDefault is the non-regression gate: with the env flag unset the feature is
// off (no warmer goroutine, no fan-out); it arms only on the explicit "true".
func TestPlaylistPushDisabledByDefault(t *testing.T) {
	if playlistPushEnabled() {
		t.Fatal("playlist push enabled with FAAS_PLAYLIST_PUSH unset — must be default-off (K10/HOTP)")
	}
	t.Setenv("FAAS_PLAYLIST_PUSH", "true")
	if !playlistPushEnabled() {
		t.Fatal("playlist push not enabled with FAAS_PLAYLIST_PUSH=true")
	}
	t.Setenv("FAAS_PLAYLIST_PUSH", "false")
	if playlistPushEnabled() {
		t.Fatal("playlist push enabled with FAAS_PLAYLIST_PUSH=false")
	}
}
