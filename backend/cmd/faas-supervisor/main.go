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
	}

	renderSock := env("RENDER_SOCK", "/run/faas/render.sock")
	_ = os.Remove(renderSock)
	l, err := net.Listen("unix", renderSock)
	if err != nil {
		log.Fatalf("faas-supervisor: render listen %s: %v", renderSock, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("POST /render", s.handleRender)

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
	res := renderOnce(ctx, s.pool, s.box, fn, rctx, RenderOpts{
		Secrets:          SecretsReal,
		Limits:           s.limits,
		Force:            body.Force,
		DitherDefault:    s.ditherDefault,
		M4Sock:           s.m4Sock,
		Timeout:          time.Duration(s.limits.TimeoutMs)*time.Millisecond + 10*time.Second,
		EgressRegister:   s.egress.register,
		EgressUnregister: s.egress.unregister,
	})
	if res.Err != nil {
		// W4a: no cache/last-good yet — stage-3 error frame straight away (stages 1-2 arrive in W4b).
		s.writeFrame(w, errorFrame, "error", true, s.retryWake)
		return
	}
	s.writeFrame(w, res.Packed, "ok", false, s.wakeFor(res.Meta))
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
