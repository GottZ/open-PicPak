package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/open-picpak/backend/internal/imgstore"
	"github.com/open-picpak/backend/internal/playliststore"
	"github.com/open-picpak/backend/internal/plrender"
)

// playlistRenderFunc is the injectable drive seam for the built-in __playlist render: production wires
// s.doRenderPlaylist (the real M4 drive with the injected source image); tests inject a counting stub
// so the variant-cache and single-flight gates observe worker drives without a live worker.
type playlistRenderFunc func(ctx context.Context, source string, input []byte, dither string) RenderResult

// playlistFit whitelists the sharp resize fit templated into the built-in source. fit already comes
// from the CHECK-constrained playlist_item.fit column, so this is belt-and-suspenders against a
// future schema change leaking an arbitrary value into the generated JS.
var playlistFit = map[string]bool{"cover": true, "contain": true, "fill": true}

// playlistSource builds the built-in __playlist worker source: a FIXED, server-authored JS constant
// (NOT operator code — a playlist is server-owned policy data, so no eval surface is opened, §4.3).
// The per-item `fit` is baked in as a validated literal; the source decodes ctx.input (the injected
// source image) and resizes to the exact 400x300 panel geometry the rasteriser requires (off-size is
// a render error — the source owns the fit, the supervisor never re-fits, D24.3).
//
// It round-trips the resize through a raw 400x300x3 buffer and re-wraps it: the rasteriser reads
// sharp.metadata() BEFORE toBuffer(), and a pending .resize() does NOT change metadata() (which
// reports the INPUT dimensions), so a bare `.resize(400,300)` image would fail the off-size check.
// Re-wrapping the raw buffer makes metadata() report the true 400x300.
func playlistSource(fit string) string {
	if !playlistFit[fit] {
		fit = "cover"
	}
	return fmt.Sprintf(`export default async (ctx, cap) => {
  const b = await cap.sharp(ctx.input).resize(400, 300, { fit: %q }).removeAlpha().raw().toBuffer();
  return { image: cap.sharp(b, { raw: { width: 400, height: 300, channels: 3 } }) };
}`, fit)
}

// singleFlight collapses concurrent renders of the SAME content variant (image_sha, fit, dither) into
// ONE worker drive: the first caller renders, every concurrent caller on the same key waits for that
// result (§4.3 cache-stampede guard — N cold devices on a fresh variant must not each drive the 4
// worker slots to a mass unavailable.bin). It is a supervisor-lifetime map; the durable dedup is the
// content-addressed frame_variant_cache, this is only the in-flight window.
type singleFlight struct {
	mu    sync.Mutex
	calls map[string]*flightCall
}

type flightCall struct {
	done   chan struct{}
	packed []byte
	err    error
}

func newSingleFlight() *singleFlight { return &singleFlight{calls: map[string]*flightCall{}} }

// do runs fn once per key while a call is in flight; concurrent callers on the same key block on the
// leader's result. The bool reports whether THIS caller was the leader (drove the worker) — the
// stampede gate asserts exactly one leader across N concurrent misses.
func (sf *singleFlight) do(key string, fn func() ([]byte, error)) (packed []byte, err error, leader bool) {
	sf.mu.Lock()
	if c, ok := sf.calls[key]; ok {
		sf.mu.Unlock()
		<-c.done
		return c.packed, c.err, false
	}
	c := &flightCall{done: make(chan struct{})}
	sf.calls[key] = c
	sf.mu.Unlock()

	c.packed, c.err = fn()
	close(c.done)

	sf.mu.Lock()
	delete(sf.calls, key)
	sf.mu.Unlock()
	return c.packed, c.err, true
}

// buildPlaylistFrame is the built-in __playlist render path (§4.3). It resolves the rotation cursor
// (single-advance under the per-serial advisory lock), picks the current item, serves the fleet-shared
// packed variant on a cache hit, or renders once (single-flighted) on a miss and persists the variant.
// It ALWAYS returns a valid 30000-byte frame; a fail-closed case (empty playlist, missing item/image,
// render failure with no cached fallback) serves the embedded error frame + a retry wake.
func (s *supervisor) buildPlaylistFrame(ctx context.Context, serial string, playlistID int64, now time.Time) (packed []byte, status string, stale bool, wake int) {
	pl, err := playliststore.GetPlaylist(ctx, s.pool, playlistID)
	if err != nil {
		log.Printf("faas: playlist %d get %s: %v", playlistID, serial, err)
		return errorFrame, "error", true, s.retryWake
	}
	items, err := playliststore.RotationItems(ctx, s.pool, playlistID)
	if err != nil {
		log.Printf("faas: playlist %d items %s: %v", playlistID, serial, err)
		return errorFrame, "error", true, s.retryWake
	}
	if len(items) == 0 {
		return errorFrame, "error", true, s.retryWake // empty playlist — fail-closed (§5)
	}

	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	order := plrender.RotationOrder(pl.OrderMode, pl.ShuffleEpoch, serial, playlistID, ids)

	interval := time.Duration(pl.IntervalS) * time.Second
	cur, err := plrender.ResolveCursor(ctx, s.pool, serial, playlistID, interval, len(items), now)
	if err != nil {
		log.Printf("faas: playlist %d cursor %s: %v", playlistID, serial, err)
		return errorFrame, "error", true, s.retryWake
	}
	item := items[order[cur.Index]]

	img, err := imgstore.GetImage(ctx, s.pool, item.ImageID)
	if err != nil {
		log.Printf("faas: playlist %d image %d %s: %v", playlistID, item.ImageID, serial, err)
		return errorFrame, "error", true, s.playlistWake(now, interval, cur)
	}

	packed, err = s.packedVariant(ctx, img, item)
	if err != nil {
		log.Printf("faas: playlist %d render %s (sha %s fit %s): %v", playlistID, serial, img.Sha256, item.Fit, err)
		return errorFrame, "error", true, s.retryWake
	}
	return packed, "ok", false, s.playlistWake(now, interval, cur)
}

// packedVariant returns the packed 30000-byte frame for an (image, fit, dither) variant: a
// fleet-shared cache hit serves without touching the worker (pack once, serve N); a miss renders ONCE
// through the single-flight, then persists the variant. The single-flight key IS the cache key, so
// concurrent misses on the same variant collapse to one worker drive.
func (s *supervisor) packedVariant(ctx context.Context, img imgstore.Image, item playliststore.PlaylistItem) ([]byte, error) {
	if packed, err := plrender.VariantGet(ctx, s.pool, img.Sha256, item.Fit, item.Dither); err == nil {
		return packed, nil // fleet-shared hit — no worker drive (§4.3 step 3)
	}

	key := img.Sha256 + "\x00" + item.Fit + "\x00" + item.Dither
	packed, err, _ := s.plFlight.do(key, func() ([]byte, error) {
		// Re-check the cache inside the flight: a leader that finished between our miss and acquiring
		// the flight already persisted it.
		if p, err := plrender.VariantGet(ctx, s.pool, img.Sha256, item.Fit, item.Dither); err == nil {
			return p, nil
		}
		blob, _, err := imgstore.LoadBlob(ctx, s.pool, s.imgBlobDir, img.ID)
		if err != nil {
			return nil, fmt.Errorf("load blob: %w", err)
		}
		res := s.renderPlaylist(ctx, playlistSource(item.Fit), blob, item.Dither)
		if res.Err != nil {
			return nil, fmt.Errorf("render %s", res.Err.Kind)
		}
		if err := plrender.VariantPut(ctx, s.pool, img.Sha256, item.Fit, item.Dither, res.Packed); err != nil {
			return nil, fmt.Errorf("variant put: %w", err)
		}
		return res.Packed, nil
	})
	return packed, err
}

// playlistWake computes the device's next wake for the playlist path: the sooner of the day/night
// cadence and the time left until the next rotation advance, jittered per device to break the
// thundering herd (K12), then clamped to [60, MaxWake]. A device thus wakes near its rotation
// boundary, never below the 60s floor, and never all at the same instant fleet-wide.
func (s *supervisor) playlistWake(now time.Time, interval time.Duration, cur plrender.Cursor) int {
	base := s.wake.NextWakeSeconds(now)
	if toNext := int((interval - now.Sub(cur.AdvancedAt)).Seconds()); toNext > 0 && toNext < base {
		base = toNext
	}
	return clampWake(s.jitter(base), s.wake.MaxWake)
}

// jitter spreads a wake EARLIER by up to jitterFrac (default 15%) of its value, seeded from the
// supervisor's rng (tests inject a deterministic source). Spreading only earlier keeps the wake within
// the intended cadence / night sleep (never later), while decorrelating fleet-synchronised wakes.
func (s *supervisor) jitter(secs int) int {
	frac := s.jitterFrac
	if frac <= 0 {
		frac = 0.15
	}
	r := s.rng
	if r == nil {
		r = rand.Float64
	}
	return secs - int(r()*frac*float64(secs))
}
