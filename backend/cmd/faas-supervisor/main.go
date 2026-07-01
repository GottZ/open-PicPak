// Command faas-supervisor is the TRUSTED FaaS process (holds SECRETS_KEY + the DB). It fronts the
// device frame path for cmd/ingest over the render-request seam (HTTP-over-UDS), resolves secrets,
// drives the untrusted Bun worker over M4, and packs BWRY. It is the ONLY FaaS-zone process on
// dbnet (M5/D18.4). This W4a build renders per request with a stage-3 error-frame fallback; the
// TTL cache + durable last-good (stages 1-2) + cron/prerender fan-out land in W4b.
package main

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

type supervisor struct {
	pool          *pgxpool.Pool
	box           *sealbox.Box
	wake          WakeConfig
	m4Sock        string
	ditherDefault string
	limits        faasproto.Limits
	retryWake     int
	egress        *egressClient
	cache         *frameCache
	render        renderFunc         // s.doRender in prod; a stub in tests
	renderTTL     time.Duration      // sync hot-cache TTL default (RENDER_TTL)
	schedTick     time.Duration      // scheduler scan cadence
	lastRun       map[int64]time.Time // fn id -> last fan-out (scheduler goroutine only)
}

func main() {
	ctx := context.Background()
	pool := openPool(ctx)
	defer pool.Close()

	box, err := sealbox.FromEnv() // SECRETS_KEY lives here (+ cmd/admin only, M5)
	if err != nil {
		log.Fatalf("faas-supervisor: sealbox: %v", err)
	}

	s := &supervisor{
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
	}
	s.render = s.doRender // the real M4 render drive (tests inject a stub)
	go s.runScheduler(ctx)

	renderSock := env("RENDER_SOCK", "/run/faas/render.sock")
	_ = os.Remove(renderSock)
	l, err := net.Listen("unix", renderSock)
	if err != nil {
		log.Fatalf("faas-supervisor: render listen %s: %v", renderSock, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /render", s.handleRender)
	mux.HandleFunc("POST /webhook", s.handleWebhookFanout)

	// admin→supervisor test-render seam (Doc 25) — a SEPARATE UDS (dbnet, admin↔supervisor) from the
	// ingest render seam, so ingest can never reach test-render and admin can never reach /render. Opt-in
	// via FAAS_TEST_SOCK (unset → no test-render arm, pausability-safe; cmd/admin then 503s the route).
	if testSock := env("FAAS_TEST_SOCK", ""); testSock != "" {
		_ = os.Remove(testSock)
		tl, terr := net.Listen("unix", testSock)
		if terr != nil {
			log.Fatalf("faas-supervisor: test-render listen %s: %v", testSock, terr)
		}
		// cross-UID: admin (a different user) connects to this socket the supervisor (65534) created —
		// chmod 0777 so the connect is permitted, mirroring the worker's m4.sock (compose faas-sock-init).
		_ = os.Chmod(testSock, 0o777)
		tmux := http.NewServeMux()
		tmux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
		tmux.HandleFunc("POST /test-render", s.handleTestRender)
		go func() {
			log.Printf("faas-supervisor: test-render seam on %s", testSock)
			log.Fatalf("faas-supervisor: test-render serve: %v",
				(&http.Server{Handler: tmux, ReadHeaderTimeout: 10 * time.Second}).Serve(tl))
		}()
	}

	log.Printf("faas-supervisor: render-request seam on %s, M4 %s", renderSock, s.m4Sock)
	log.Fatalf("faas-supervisor: serve: %v", (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(l))
}

func openPool(ctx context.Context) *pgxpool.Pool {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			env("POSTGRES_USER", "picpak"), os.Getenv("POSTGRES_PASSWORD"),
			env("PGHOST", "timescaledb:5432"), env("POSTGRES_DB", "picpak"))
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("faas-supervisor: db pool: %v", err)
	}
	return pool
}

// handleRender is the ingest→supervisor render-request seam (§4.3). It returns a body ALWAYS
// (a valid 30000-byte frame) + the wake/status/stale headers; ingest layers the OTA signal on top.
func (s *supervisor) handleRender(w http.ResponseWriter, r *http.Request) {
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

	ctx := r.Context()
	fnID, ok, err := faasstore.BoundFunctionID(ctx, s.pool, body.Serial)
	if err != nil {
		log.Printf("faas-supervisor: bound lookup %s: %v", body.Serial, err)
		s.writeFrame(w, errorFrame, "error", true, s.retryWake)
		return
	}
	if !ok {
		// no function bound to this device — nothing to render; serve the error frame (retry soon).
		s.writeFrame(w, errorFrame, "error", true, s.retryWake)
		return
	}
	fn, err := faasstore.LoadFunction(ctx, s.pool, fnID)
	if err != nil {
		if !errors.Is(err, faasstore.ErrNotFound) {
			log.Printf("faas-supervisor: load fn %d: %v", fnID, err)
		}
		s.writeFrame(w, errorFrame, "error", true, s.retryWake)
		return
	}

	rctx := faasproto.RequestCtx{
		Serial:  body.Serial,
		Channel: body.Channel,
		Trigger: faasproto.Trigger{Type: triggerOrRender(body.Trigger)},
		Now:     nowOrDefault(body.Now),
	}
	// Trigger routing (D24.2/D24.12): render/sync renders inline (TTL cache + fallback); every other
	// trigger (render/prerender, schedule, webhook) serves the last-good the timer/cron wrote — the
	// device request never drives the worker inline.
	var (
		packed []byte
		status string
		stale  bool
		wake   int
	)
	if s.syncInline(fn) {
		packed, status, stale, wake = s.buildFrame(ctx, fn, rctx, body.Force)
	} else {
		packed, status, stale, wake = s.serveLastGood(ctx, fn, rctx)
	}
	s.writeFrame(w, packed, status, stale, wake)
}

// handleWebhookFanout is the ingest→supervisor webhook seam (§4.3). ingest has ALREADY validated the
// per-function token (sha256 + crypto/subtle.ConstantTimeCompare, D24.13) — this internal UDS endpoint
// fans the named function out over EVERY bound serial (D24.12), the POST body carried verbatim as
// ctx.trigger.payload. RENDER_SOCK is reachable ONLY by ingest (shared-volume UDS), so the caller is
// trusted; the trigger_type/enabled re-check is cheap defense-in-depth, NOT the auth gate. Fan-out is
// ASYNC: a webhook must not block on N renders (nor couple to ingest's client timeout) — the device
// picks up the new last-good on its next /frame, so 202 the moment the function is verified and the
// fan-out is launched. The goroutine uses context.Background() to outlive this request; a process
// restart drops an in-flight fan-out (best-effort — the next webhook or device poll recovers).
func (s *supervisor) handleWebhookFanout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.Name == "" {
		http.Error(w, "bad webhook request", http.StatusBadRequest)
		return
	}
	fn, err := faasstore.LoadFunctionByName(r.Context(), s.pool, body.Name)
	if err != nil {
		// ingest looked it up moments ago, so a miss here is a delete race/misconfig — 404, no fan-out.
		http.Error(w, "no such function", http.StatusNotFound)
		return
	}
	if fn.TriggerType != faasstore.TriggerWebhook || !fn.Enabled {
		http.Error(w, "not an enabled webhook function", http.StatusConflict)
		return
	}
	go s.fanOut(context.Background(), fn, faasproto.Trigger{Type: "webhook", Payload: body.Payload})
	w.WriteHeader(http.StatusAccepted)
}

// runScheduler periodically fans out the enabled prerender/schedule functions whose interval has
// elapsed (M1: interval-based; // TODO(cron): full 5-field cron for schedule triggers).
func (s *supervisor) runScheduler(ctx context.Context) {
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

func (s *supervisor) tickSchedule(ctx context.Context) {
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

func (s *supervisor) wakeFor(meta faasproto.ResponseMeta) int {
	if meta.NextWakeHint != nil {
		return s.wake.ClampHint(*meta.NextWakeHint)
	}
	return s.wake.NextWakeSeconds(time.Now())
}

func (s *supervisor) writeFrame(w http.ResponseWriter, packed []byte, status string, stale bool, wake int) {
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
