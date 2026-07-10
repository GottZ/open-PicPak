package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
)

// triggerConfig is the per-function policy (Policy=Data) parsed from faas_functions.trigger_config.
type triggerConfig struct {
	Mode      string `json:"mode"`       // render trigger: "sync" (default) | "prerender"
	TTLSec    int    `json:"ttl_s"`      // hot-cache TTL for sync (default s.renderTTL)
	IntervalS int    `json:"interval_s"` // prerender/schedule cadence
	Dither    string `json:"dither"`
	Cron      string `json:"cron"` // schedule // TODO(cron): full 5-field parsing; M1 uses interval_s
}

func parseTriggerConfig(raw json.RawMessage) triggerConfig {
	var c triggerConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

// renderFunc is the injectable drive seam: production wires s.doRender (real M4 render); tests inject
// a stub so build_frame's cache/last-good/fallback and the fan-out are exercised without a worker.
type renderFunc func(ctx context.Context, fn *faasstore.Function, rctx faasproto.RequestCtx, force bool) RenderResult

func (s *supervisor) doRender(ctx context.Context, fn *faasstore.Function, rctx faasproto.RequestCtx, force bool) RenderResult {
	return renderOnce(ctx, s.pool, s.box, fn, rctx, RenderOpts{
		Secrets:          SecretsReal,
		Limits:           s.limits,
		Force:            force,
		Dither:           parseTriggerConfig(fn.TriggerConfig).Dither, // trusted per-function policy (A31.2)
		DitherDefault:    s.ditherDefault,
		M4Sock:           s.m4Sock,
		Timeout:          time.Duration(s.limits.TimeoutMs)*time.Millisecond + 10*time.Second,
		EgressRegister:   s.egress.register,
		EgressUnregister: s.egress.unregister,
	})
}

// doRenderPlaylist drives the built-in __playlist source over M4 with the source image injected as
// the request's `input` field (A27 W3). The synthetic function has no secrets and no egress, so it
// resolves nothing and provisions nothing; renderOnce packs the worker's raw RGB to the 30000-byte
// frame exactly as for an operator render (one render primitive, K7).
func (s *supervisor) doRenderPlaylist(ctx context.Context, source string, input []byte, dither string) RenderResult {
	fn := &faasstore.Function{Source: source}
	rctx := faasproto.RequestCtx{Trigger: faasproto.Trigger{Type: "render"}, Now: nowOrDefault("")}
	return renderOnce(ctx, s.pool, s.box, fn, rctx, RenderOpts{
		Secrets:       SecretsReal,
		Limits:        s.limits,
		DitherDefault: dither,
		M4Sock:        s.m4Sock,
		Timeout:       time.Duration(s.limits.TimeoutMs)*time.Millisecond + 10*time.Second,
		Input:         input,
	})
}

// --- hot cache (in-memory TTL, keyed by (serial, fn); the entry carries the source version so a
// version bump is an automatic miss — K7 cache-bust) ---

type frameKey struct {
	serial string
	fnID   int64
}

type frameEntry struct {
	version    int
	packed     []byte
	renderedAt time.Time
}

type frameCache struct {
	mu sync.Mutex
	m  map[frameKey]frameEntry
}

func newFrameCache() *frameCache { return &frameCache{m: map[frameKey]frameEntry{}} }

func (c *frameCache) get(serial string, fnID int64) (frameEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[frameKey{serial, fnID}]
	return e, ok
}

func (c *frameCache) put(serial string, fnID int64, version int, packed []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[frameKey{serial, fnID}] = frameEntry{version: version, packed: packed, renderedAt: s_now()}
}

// s_now is a package-level clock so tests can keep it real; kept trivial (no injection needed yet).
func s_now() time.Time { return time.Now() }

// syncInline reports whether a device GET /frame renders inline (render/sync) vs serves last-good
// (render/prerender, schedule, webhook) — D24.2/D24.12.
func (s *supervisor) syncInline(fn *faasstore.Function) bool {
	if fn.TriggerType != faasstore.TriggerRender {
		return false
	}
	return parseTriggerConfig(fn.TriggerConfig).Mode != "prerender"
}

func (s *supervisor) ttlFor(fn *faasstore.Function) time.Duration {
	if c := parseTriggerConfig(fn.TriggerConfig); c.TTLSec > 0 {
		return time.Duration(c.TTLSec) * time.Second
	}
	return s.renderTTL
}

// buildFrame is the sync render path: hot-cache short-circuit (K7 version-keyed) → renderOnce →
// durable last-good + cache on success → the 3-stage fallback on failure (warm cache → DB last-good →
// embedded error frame). It NEVER crashes the response and always returns a valid 30000-byte frame.
func (s *supervisor) buildFrame(ctx context.Context, fn *faasstore.Function, rctx faasproto.RequestCtx, force bool) (packed []byte, status string, stale bool, wake int) {
	if !force {
		if e, ok := s.cache.get(rctx.Serial, fn.ID); ok && e.version == fn.Version && time.Since(e.renderedAt) < s.ttlFor(fn) {
			return e.packed, "ok", false, s.wakeDefault() // cache hit — no worker drive (protects upstream fetch, T6)
		}
	}
	res := s.render(ctx, fn, rctx, force)
	if res.Err == nil {
		s.cache.put(rctx.Serial, fn.ID, fn.Version, res.Packed)
		if err := faasstore.LastGoodPut(ctx, s.pool, rctx.Serial, fn.ID, res.Packed, "ok"); err != nil {
			log.Printf("faas: last-good put %s fn %d: %v", rctx.Serial, fn.ID, err)
		}
		return res.Packed, "ok", false, s.wakeFor(res.Meta)
	}
	// 3-stage fallback (D24.6), in order:
	if e, ok := s.cache.get(rctx.Serial, fn.ID); ok && e.packed != nil { // (1) warm in-memory frame
		return e.packed, "stale", true, s.retryWake
	}
	if lg, err := faasstore.LastGoodGet(ctx, s.pool, rctx.Serial, fn.ID); err == nil && lg != nil { // (2) durable last-good
		return lg.Packed, "stale", true, s.retryWake
	}
	return errorFrame, "error", true, s.retryWake // (3) embedded error frame, never calls the worker
}

// serveLastGood is the non-sync path (prerender/schedule/webhook): the device request always serves
// the durable last-good (written by the timer/cron), never driving the worker inline (D24.12).
func (s *supervisor) serveLastGood(ctx context.Context, fn *faasstore.Function, rctx faasproto.RequestCtx) (packed []byte, status string, stale bool, wake int) {
	if lg, err := faasstore.LastGoodGet(ctx, s.pool, rctx.Serial, fn.ID); err == nil && lg != nil {
		return lg.Packed, lg.Status, lg.Status != "ok", s.wakeDefault()
	}
	return errorFrame, "error", true, s.retryWake // nothing rendered yet
}

// fanOut renders a function for EVERY serial bound to it (D24.12), each with that serial as ctx.serial,
// writing per-(serial, function) last-good. This is the writer for prerender/schedule/webhook triggers.
// A per-serial failure leaves that serial's prior last-good intact (never overwrites good with error).
func (s *supervisor) fanOut(ctx context.Context, fn *faasstore.Function, trigger faasproto.Trigger) (ok, failed int) {
	serials, err := faasstore.BoundSerials(ctx, s.pool, fn.ID)
	if err != nil {
		log.Printf("faas: fanOut bound serials fn %d: %v", fn.ID, err)
		return 0, 0
	}
	for _, serial := range serials {
		rctx := faasproto.RequestCtx{
			Serial:  serial,
			Channel: s.channelOf(ctx, serial),
			Trigger: trigger,
			Now:     nowOrDefault(""),
		}
		res := s.render(ctx, fn, rctx, false)
		if res.Err != nil {
			log.Printf("faas: fanOut render %s fn %d: %s", serial, fn.ID, res.Err.Kind)
			failed++
			continue
		}
		if err := faasstore.LastGoodPut(ctx, s.pool, serial, fn.ID, res.Packed, "ok"); err != nil {
			log.Printf("faas: fanOut last-good put %s fn %d: %v", serial, fn.ID, err)
			failed++
			continue
		}
		s.cache.put(serial, fn.ID, fn.Version, res.Packed)
		ok++
	}
	return ok, failed
}

// channelOf reads a device's operator-owned channel (server-side, D20.5). Empty if unknown/absent.
func (s *supervisor) channelOf(ctx context.Context, serial string) string {
	var ch string
	_ = s.pool.QueryRow(ctx, `SELECT channel FROM devices WHERE serial = $1`, serial).Scan(&ch)
	return ch
}

func (s *supervisor) wakeDefault() int { return s.wake.NextWakeSeconds(time.Now()) }
