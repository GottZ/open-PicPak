// Command faas-supervisor is the TRUSTED FaaS process (holds SECRETS_KEY + the DB). It fronts the
// device frame path for cmd/ingest over the render-request seam (HTTP-over-UDS), resolves secrets,
// drives the untrusted Bun worker over M4, and packs BWRY. It is the ONLY FaaS-zone process on
// dbnet (M5/D18.4). This W4a build renders per request with a stage-3 error-frame fallback; the
// TTL cache + durable last-good (stages 1-2) + cron/prerender fan-out land in W4b.
package faascore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/faasproto"
	"github.com/open-picpak/backend/internal/faasstore"
	"github.com/open-picpak/backend/internal/plrender"
	"github.com/open-picpak/backend/internal/sealbox"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("faas-supervisor: %s=%q invalid, using %d", k, v, def)
	}
	return def
}

type Supervisor struct {
	pool          *pgxpool.Pool
	box           *sealbox.Box
	wake          WakeConfig
	m4Sock        string
	ditherDefault string
	limits        faasproto.Limits
	retryWake     int
	egress        *egressClient
	cache         *frameCache
	render        renderFunc          // s.doRender in prod; a stub in tests
	renderTTL     time.Duration       // sync hot-cache TTL default (RENDER_TTL)
	schedTick     time.Duration       // scheduler scan cadence
	lastRun       map[int64]time.Time // fn id -> last fan-out (scheduler goroutine only)

	// A27 W3 playlist render anchor.
	renderPlaylist playlistRenderFunc // s.doRenderPlaylist in prod; a counting stub in tests
	plFlight       *singleFlight      // per-variant in-flight dedup (cache-stampede guard)
	imgBlobDir     string             // imgblobs volume, :ro (source-image reads for the built-in source)
	jitterFrac     float64            // X-Next-Wake jitter fraction (K12); 0 → default 0.15
	rng            func() float64     // jitter randomness; nil → math/rand (tests inject determinism)

	// A27 W4 pre-pack warmer.
	warmLimit int // max concurrent pre-pack variant renders (K5 staffel); < 1 → 1
}

// New builds the in-process Supervisor for the merged cmd/backend (design 35 §2). pool + box are owned
// by cmd/backend (ONE pool, the M5 address-space split collapsed by A35). The FaaS env union is read
// here (design 35 §7 checklist): NIGHT_START_HOUR / NIGHT_END_HOUR / DAY_INTERVAL / MAX_WAKE /
// FAAS_M4_SOCK / DITHER_DEFAULT / FAAS_TIMEOUT_MS / FAAS_MEM_MB / RETRY_WAKE / EGRESS_CONTROL_SOCK /
// RENDER_TTL / FAAS_SCHED_TICK / IMG_BLOB_DIR / FAAS_WARM_LIMIT. There is NO render.sock / test.sock
// listener anymore: ingest reaches Render/Fanout and admin reaches TestRenderSeam by direct method call.
func New(pool *pgxpool.Pool, box *sealbox.Box) *Supervisor {
	s := &Supervisor{
		pool: pool,
		box:  box,
		wake: WakeConfig{
			NightStartHour: envInt("NIGHT_START_HOUR", 23),
			NightEndHour:   envInt("NIGHT_END_HOUR", 6),
			DayInterval:    envInt("DAY_INTERVAL", 3600),
			MaxWake:        envInt("MAX_WAKE", 8*3600),
		},
		m4Sock:        env("FAAS_M4_SOCK", "/run/faas/m4.sock"),
		ditherDefault: env("DITHER_DEFAULT", "none"),
		limits:        faasproto.Limits{TimeoutMs: envInt("FAAS_TIMEOUT_MS", 8000), MemMB: envInt("FAAS_MEM_MB", 256)},
		retryWake:     envInt("RETRY_WAKE", 600),
		egress:        newEgressClient(env("EGRESS_CONTROL_SOCK", "")),
		cache:         newFrameCache(),
		renderTTL:     time.Duration(envInt("RENDER_TTL", 120)) * time.Second,
		schedTick:     time.Duration(envInt("FAAS_SCHED_TICK", 30)) * time.Second,
		lastRun:       map[int64]time.Time{},
		plFlight:      newSingleFlight(),
		imgBlobDir:    env("IMG_BLOB_DIR", "/var/lib/picpak/imgblobs"),
		jitterFrac:    0.15,
		warmLimit:     envInt("FAAS_WARM_LIMIT", 2),
	}
	s.render = s.doRender                 // the real M4 render drive (tests inject a stub)
	s.renderPlaylist = s.doRenderPlaylist // the built-in __playlist M4 drive (tests inject a stub)
	return s
}

// Start launches the supervisor's background LISTEN goroutines: the scheduler scan loop and — only when
// FAAS_PLAYLIST_PUSH is armed (default-off, K10/HOTP) — the pre-pack warmer + refresh fan-out. These are
// the two permanent LISTEN-holder connections the merged pool reserves for (design 35 §6).
func (s *Supervisor) Start(ctx context.Context) {
	go s.runScheduler(ctx)
	if playlistPushEnabled() {
		go s.runPlaylistWarmer(ctx)
		log.Printf("backend: faas playlist push (pre-pack warmer + refresh fan-out) enabled")
	}
}

// TestRenderArmed reports whether the test-render/preview arm is enabled (BACKEND_TEST_RENDER_ENABLED).
// It replaces the old FAAS_TEST_SOCK-presence gate: unset ⇒ admin test-run 503s and preview degrades.
func TestRenderArmed() bool { return os.Getenv("BACKEND_TEST_RENDER_ENABLED") == "true" }

// Render is the in-process render-request seam (design 35 §3, replaces POST /render over render.sock).
// It ALWAYS returns a valid 30000-byte frame + the wake/status/stale triplet; the ingest frame handler
// layers Doc 20's OTA signal on top. Byte-identical to the old handleRender logic (§4.3 preserved).
func (s *Supervisor) Render(ctx context.Context, serial, channel, trigger, now string, force bool) (packed []byte, status string, stale bool, wake int) {
	// A27 W3: resolve BOTH bindings in one round-trip (§4.3). A playlist binding takes precedence
	// (playlist-first); the advisory-locked bind paths guarantee at most one binding exists, so this
	// ordering is defense-in-depth, not the correctness anchor.
	bind, err := plrender.ResolveBinding(ctx, s.pool, serial)
	if err != nil {
		log.Printf("faas-supervisor: resolve binding %s: %v", serial, err)
		return errorFrame, "error", true, s.retryWake
	}
	if bind.PlaylistID != 0 {
		return s.buildPlaylistFrame(ctx, serial, bind.PlaylistID, time.Now())
	}
	if bind.FunctionID == 0 {
		// no function AND no playlist bound to this device — nothing to render; serve the error frame.
		return errorFrame, "error", true, s.retryWake
	}
	fnID := bind.FunctionID
	fn, err := faasstore.LoadFunction(ctx, s.pool, fnID)
	if err != nil {
		if !errors.Is(err, faasstore.ErrNotFound) {
			log.Printf("faas-supervisor: load fn %d: %v", fnID, err)
		}
		return errorFrame, "error", true, s.retryWake
	}

	rctx := faasproto.RequestCtx{
		Serial:  serial,
		Channel: channel,
		Trigger: faasproto.Trigger{Type: triggerOrRender(trigger)},
		Now:     nowOrDefault(now),
	}
	// K15 — unified enabled semantics: enabled=false = frozen. A disabled function is served its
	// durable last-good flagged stale WITHOUT rendering inline and WITHOUT driving the worker (no
	// last-good → error frame + retry-wake). This aligns the sync render path with the webhook-409 and
	// scheduler-skip, which already gate on fn.Enabled; before K15 this path rendered disabled
	// functions regardless. Checked here, immediately after LoadFunction, so neither buildFrame nor
	// serveLastGood can drive the worker for a frozen function.
	if !fn.Enabled {
		return s.serveFrozen(ctx, fn, rctx)
	}
	// Trigger routing (D24.2/D24.12): render/sync renders inline (TTL cache + fallback); every other
	// trigger (render/prerender, schedule, webhook) serves the last-good the timer/cron wrote — the
	// device request never drives the worker inline.
	if s.syncInline(fn) {
		return s.buildFrame(ctx, fn, rctx, force)
	}
	return s.serveLastGood(ctx, fn, rctx)
}

// handleRender is the thin HTTP wrapper retained for the in-package tests (enabled_test etc.) that drive
// the render path via httptest. Production reaches Render() directly (no render.sock).
func (s *Supervisor) handleRender(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Serial  string `json:"serial"`
		Channel string `json:"channel"`
		Trigger string `json:"trigger"`
		Now     string `json:"now"`
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Serial == "" {
		http.Error(w, "bad render request", http.StatusBadRequest)
		return
	}
	packed, status, stale, wake := s.Render(r.Context(), body.Serial, body.Channel, body.Trigger, body.Now, body.Force)
	s.writeFrame(w, packed, status, stale, wake)
}

// ErrWebhookNotFound / ErrWebhookNotEnabled are the Fanout sentinels the ingest webhook handler maps
// back to 404 / 503 (the caller already validated the per-function token before reaching here).
var (
	ErrWebhookNotFound   = errors.New("no such function")
	ErrWebhookNotEnabled = errors.New("not an enabled webhook function")
)

// Fanout is the in-process webhook seam (design 35 §3, replaces POST /webhook over render.sock). The
// ingest webhook handler has ALREADY validated the per-function token (sha256 + constant-time, D24.13);
// this launches the fan-out over EVERY bound serial (D24.12), the payload carried verbatim as
// ctx.trigger.payload. The trigger_type/enabled re-check is cheap defense-in-depth, NOT the auth gate.
// Fan-out is ASYNC (a webhook must not block on N renders); the goroutine uses context.Background() to
// outlive the request. Returns nil once the fan-out is launched (the ingest handler then 202s).
func (s *Supervisor) Fanout(ctx context.Context, name string, payload json.RawMessage) error {
	fn, err := faasstore.LoadFunctionByName(ctx, s.pool, name)
	if err != nil {
		// ingest looked it up moments ago, so a miss here is a delete race/misconfig — no fan-out.
		return ErrWebhookNotFound
	}
	if fn.TriggerType != faasstore.TriggerWebhook || !fn.Enabled {
		return ErrWebhookNotEnabled
	}
	go s.fanOut(context.Background(), fn, faasproto.Trigger{Type: "webhook", Payload: payload})
	return nil
}

// handleWebhookFanout is the thin HTTP wrapper retained for the in-package tests. Production reaches
// Fanout() directly (no render.sock).
func (s *Supervisor) handleWebhookFanout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad webhook request", http.StatusBadRequest)
		return
	}
	switch err := s.Fanout(r.Context(), body.Name, body.Payload); {
	case errors.Is(err, ErrWebhookNotFound):
		http.Error(w, "no such function", http.StatusNotFound)
	case errors.Is(err, ErrWebhookNotEnabled):
		http.Error(w, "not an enabled webhook function", http.StatusConflict)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// runScheduler periodically fans out the enabled prerender/schedule functions whose interval has
// elapsed (M1: interval-based; // TODO(cron): full 5-field cron for schedule triggers).
func (s *Supervisor) runScheduler(ctx context.Context) {
	t := time.NewTicker(s.schedTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickSchedule(ctx)
		}
	}
}

func (s *Supervisor) tickSchedule(ctx context.Context) {
	sums, err := faasstore.ListFunctions(ctx, s.pool)
	if err != nil {
		log.Printf("faas-supervisor: scheduler list: %v", err)
		return
	}
	now := time.Now()
	for _, sum := range sums {
		if !sum.Enabled {
			continue
		}
		fn, err := faasstore.LoadFunction(ctx, s.pool, sum.ID)
		if err != nil {
			continue
		}
		cfg := parseTriggerConfig(fn.TriggerConfig)
		ttype := ""
		switch {
		case fn.TriggerType == faasstore.TriggerRender && cfg.Mode == "prerender":
			ttype = "prerender"
		case fn.TriggerType == faasstore.TriggerSchedule:
			ttype = "schedule"
		default:
			continue // sync render + webhook have no timer-driven writer
		}
		interval := cfg.IntervalS
		if interval <= 0 {
			interval = 900 // sane M1 default cadence
		}
		if last, ok := s.lastRun[fn.ID]; ok && now.Sub(last) < time.Duration(interval)*time.Second {
			continue
		}
		s.lastRun[fn.ID] = now
		s.fanOut(ctx, fn, faasproto.Trigger{Type: ttype})
	}
}

func (s *Supervisor) wakeFor(meta faasproto.ResponseMeta) int {
	if meta.NextWakeHint != nil {
		return s.wake.ClampHint(*meta.NextWakeHint)
	}
	return s.wake.NextWakeSeconds(time.Now())
}

func (s *Supervisor) writeFrame(w http.ResponseWriter, packed []byte, status string, stale bool, wake int) {
	w.Header().Set("X-Faas-Wake", strconv.Itoa(wake))
	w.Header().Set("X-Faas-Status", status)
	w.Header().Set("X-Faas-Stale", map[bool]string{true: "1", false: "0"}[stale])
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(packed)
}

func triggerOrRender(t string) string {
	if t == "" {
		return "render"
	}
	return t
}

func nowOrDefault(s string) string {
	if s == "" {
		return time.Now().UTC().Format(time.RFC3339)
	}
	return s
}

// egressClient provisions the egress proxy with a per-call cred→allowlist over its control UDS.
// All methods are nil-safe: an unset EGRESS_CONTROL_SOCK yields a nil client, so a no-egress
// deployment simply never provisions (a function that fetches would then be denied at the proxy).
type egressClient struct {
	sock string
	hc   *http.Client
}

func newEgressClient(sock string) *egressClient {
	if sock == "" {
		return nil
	}
	return &egressClient{
		sock: sock,
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

func (e *egressClient) register(cred string, allow []string) error {
	if e == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"cred": cred, "allow": allow})
	resp, err := e.hc.Post("http://egress/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("egress register: status %d", resp.StatusCode)
	}
	return nil
}

func (e *egressClient) unregister(cred string) {
	if e == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"cred": cred})
	if resp, err := e.hc.Post("http://egress/unregister", "application/json", bytes.NewReader(body)); err == nil {
		_ = resp.Body.Close()
	}
}
