package main

// Server-sent-events scaffold for GET /api/events (design 19 §4.5, D19.7).
//
// Topology. One in-process broadcast hub diffs the device roster ONCE per tick
// and fans the resulting deltas to every connection — N operator panels cost one
// poll + one diff, not N. Per connection only a bounded send mailbox + the
// initial full-roster snapshot remain. The hub shape (sseHub/sseSub +
// non-blocking broadcast + slow-client drop) is lifted from the n8n/ctxd
// internal/handler/events.go gold standard — NOT from c2notify.go, whose
// payload-free close-and-replace wakeup is the wrong shape for per-client SSE.
//
// Scaffold scope: the device-roster producer is the ONLY producer (the shell's
// liveness proof). Docs 21/22/23 wire log/telemetry/berry payloads onto this
// same hub. Frame integrity is a HUB invariant: every payload is json.Marshal'd
// (which escapes any embedded newline/CRLF), so a device-supplied string can
// never inject a premature frame boundary for every connected operator.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/devicestore"
	"github.com/open-picpak/backend/internal/operator"
)

// sseConfig holds the env-driven SSE timings (Mechanism=Code / Policy=Data:
// cmd/admin's policy surface is env, there is no settings store). Read once at
// handler construction.
type sseConfig struct {
	tick        time.Duration // ADMIN_EVENTS_TICK — diff/fan-out cadence
	ping        time.Duration // ADMIN_SSE_PING — comment-keepalive cadence (< proxy read timeout)
	reauth      time.Duration // ADMIN_SSE_REAUTH — re-auth interval → revocation latency bound
	writeWindow time.Duration // ADMIN_SSE_WRITE_DL — rolling per-write deadline
	maxConn     int           // ADMIN_SSE_MAX_CONN — connection cap before 429
}

func loadSSEConfig() sseConfig {
	return sseConfig{
		tick:        envDuration("ADMIN_EVENTS_TICK", 5*time.Second),
		ping:        envDuration("ADMIN_SSE_PING", 25*time.Second),
		reauth:      envDuration("ADMIN_SSE_REAUTH", 60*time.Second),
		writeWindow: envDuration("ADMIN_SSE_WRITE_DL", 90*time.Second),
		maxConn:     envInt("ADMIN_SSE_MAX_CONN", 8),
	}
}

// sseMailbox is the per-connection send buffer. A connection whose mailbox
// overflows cannot keep up; the hub drops it (it reconnects and re-fetches the
// snapshot) rather than stall the fan-out to everyone else.
const sseMailbox = 16

// sseFrame is one fanned-out event: a name, optional id, and pre-marshalled data
// ready to write verbatim.
type sseFrame struct {
	name string
	id   string
	data []byte
}

// sseSub is one subscriber's mailbox. broadcast fans frames into ch (buffered,
// non-blocking); the connection handler drains it. done is closed by the hub
// when it drops the sub (mailbox overflow) so the handler tears down.
type sseSub struct {
	ch   chan sseFrame
	done chan struct{}
}

// sseWriter frames events onto one connection and keeps a rolling write deadline.
// Every write is mutex-serialized: a diff frame, a keepalive ping and the re-auth
// teardown can all target the same connection.
type sseWriter struct {
	w      http.ResponseWriter
	rc     *http.ResponseController
	window time.Duration
	mu     sync.Mutex
}

// newSSEWriter wraps a connection whose 200 stream header the caller has already
// committed, probes flushability and arms the first write deadline. cmd/admin
// sets no absolute WriteTimeout (cmd/ingest/main.go:65 pattern), but the rolling
// deadline is defense (D19.7, T5): a slow-loris reader or a future hardening
// commit that adds a WriteTimeout must not be able to silently truncate the
// stream. A non-flushable writer (a middleware ResponseWriter without Unwrap())
// is logged loudly — without per-frame flushes a reverse proxy 504s the stream.
func newSSEWriter(w http.ResponseWriter, window time.Duration) *sseWriter {
	sw := &sseWriter{w: w, rc: http.NewResponseController(w), window: window}
	if err := sw.rc.Flush(); err != nil {
		log.Printf("sse: response writer not flushable, stream will buffer: %v", err)
	}
	_ = sw.rollDeadline()
	return sw
}

// rollDeadline pushes the connection write deadline one window ahead.
func (sw *sseWriter) rollDeadline() error {
	return sw.rc.SetWriteDeadline(time.Now().Add(sw.window))
}

// event writes one named event frame (id optional) carrying pre-marshalled
// compact JSON. json.Marshal never emits a raw newline (control bytes are
// escaped), so a single data: line is always a valid SSE frame. Rolls the
// deadline, writes, flushes.
func (sw *sseWriter) event(name, id string, data []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_ = sw.rollDeadline()
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	if _, err := io.WriteString(sw.w, b.String()); err != nil {
		return err
	}
	return sw.rc.Flush()
}

// ping writes an SSE comment keepalive (": ping"). The client ignores it; it
// resets the fronting proxy's read timeout and the connection write deadline.
func (sw *sseWriter) ping() error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_ = sw.rollDeadline()
	if _, err := io.WriteString(sw.w, ": ping\n\n"); err != nil {
		return err
	}
	return sw.rc.Flush()
}

// sseHub multiplexes one broadcast loop over many connections.
type sseHub struct {
	life context.Context
	cfg  sseConfig
	// Injectable seams (faked in tests, bound to the real pool in newEventsHandler).
	roster       func(ctx context.Context) ([]devicestore.Row, error)
	authenticate func(ctx context.Context, token string) (operator.AuthResult, bool, error)

	mu      sync.Mutex
	subs    map[*sseSub]struct{}
	running bool
}

// subscribe registers a connection and starts the broadcast loop if idle.
// Returns ok=false at the connection cap — the caller answers 429 and the client
// degrades to polling.
func (h *sseHub) subscribe() (*sseSub, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	max := h.cfg.maxConn
	if max <= 0 {
		max = 8
	}
	if len(h.subs) >= max {
		return nil, false
	}
	s := &sseSub{ch: make(chan sseFrame, sseMailbox), done: make(chan struct{})}
	h.subs[s] = struct{}{}
	if !h.running {
		h.running = true
		go h.runLoop()
	}
	return s, true
}

// unsubscribe removes a connection. The loop notices the empty set at its next
// tick and stops itself (bounded by one tick).
func (h *sseHub) unsubscribe(s *sseSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

func (h *sseHub) subCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// broadcast fans one frame to every subscriber without blocking. A full mailbox
// means the client cannot keep up: drop it (close done + remove) so the slow
// connection cannot stall the fan-out to the rest. Deleting during range is safe.
func (h *sseHub) broadcast(f sseFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- f:
		default:
			close(s.done)
			delete(h.subs, s)
		}
	}
}

// rosterDelta is one roster change fanned to the `devices` channel. Op is
// "upsert" (new or an identity-column change) or "remove" (the device is gone).
type rosterDelta struct {
	Op     string          `json:"op"`
	Device devicestore.Row `json:"device"`
}

// rosterIdentity is the diff key: the STABLE identity columns only. last_seen is
// deliberately EXCLUDED — it advances on every telemetry push, so diffing on it
// would churn the whole roster every tick. The roster's job is identity/
// membership; per-device liveness rides Doc 22's telemetry channel (D19.7). The
// snapshot still carries last_seen as a display value; it is never a delta trigger.
func rosterIdentity(row devicestore.Row) string {
	label := ""
	if row.Label != nil {
		label = *row.Label
	}
	return row.Serial + "\x1f" + label + "\x1f" + row.Channel + "\x1f" + strconv.FormatBool(row.Bonded)
}

// runLoop is the single broadcast loop. One tick: poll the roster, diff it
// against the last-sent state on identity columns, broadcast only the changed
// rows. lastSent starts empty so the first tick re-broadcasts the current roster
// once (idempotent upserts, keyed by serial — clients converge); no roster
// change is ever missed. Stops on lifecycle cancel or one tick after the last
// subscriber leaves.
func (h *sseHub) runLoop() {
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	t := time.NewTicker(h.cfg.tick)
	defer t.Stop()

	lastSent := map[string]devicestore.Row{}
	for {
		select {
		case <-h.life.Done():
			return
		case <-t.C:
			if h.subCount() == 0 {
				return // last subscriber left — stop (no idle polling)
			}
			opCtx, cancel := context.WithTimeout(h.life, 10*time.Second)
			rows, err := h.roster(opCtx)
			cancel()
			if err != nil {
				log.Printf("sse: roster poll failed: %v", err)
				continue
			}
			cur := make(map[string]devicestore.Row, len(rows))
			for _, row := range rows {
				cur[row.Serial] = row
			}
			for _, d := range diffRoster(lastSent, cur) {
				h.broadcastDelta(d)
			}
			lastSent = cur
		}
	}
}

// diffRoster computes the roster deltas between the last-sent state and the
// current poll: an "upsert" for a new serial or an identity-column change, a
// "remove" for a vanished serial. A last_seen-only change yields NOTHING
// (rosterIdentity excludes it) — the property pinned by the W4 diff test. Pure
// so it is unit-testable without driving the broadcast loop.
func diffRoster(last, cur map[string]devicestore.Row) []rosterDelta {
	var out []rosterDelta
	for serial, row := range cur {
		old, ok := last[serial]
		if !ok || rosterIdentity(old) != rosterIdentity(row) {
			out = append(out, rosterDelta{Op: "upsert", Device: row})
		}
	}
	for serial, old := range last {
		if _, ok := cur[serial]; !ok {
			out = append(out, rosterDelta{Op: "remove", Device: old})
		}
	}
	return out
}

// broadcastDelta marshals one roster delta (HUB-side json.Marshal — frame
// integrity invariant) and fans it as a `devices` event.
func (h *sseHub) broadcastDelta(d rosterDelta) {
	data, err := json.Marshal(d)
	if err != nil {
		return
	}
	h.broadcast(sseFrame{name: "devices", data: data})
}

// eventsHandler serves GET /api/events over the shared hub.
type eventsHandler struct {
	hub *sseHub
}

// newEventsHandler wires the SSE handler. life is the process lifecycle context;
// pool drives the in-stream re-auth + the roster poll.
func newEventsHandler(life context.Context, pool *pgxpool.Pool) *eventsHandler {
	return &eventsHandler{hub: &sseHub{
		life: life,
		cfg:  loadSSEConfig(),
		roster: func(ctx context.Context) ([]devicestore.Row, error) {
			return devicestore.List(ctx, pool)
		},
		authenticate: func(ctx context.Context, token string) (operator.AuthResult, bool, error) {
			return operator.Authenticate(ctx, pool, token)
		},
		subs: map[*sseSub]struct{}{},
	}}
}

// handle serves the operator SSE stream. auth-gated upstream (any valid key, O1).
// Flow: cap-check → commit stream header → initial full-roster snapshot → fan-out
// deltas / pings / periodic re-auth until the client disconnects, the server
// shuts down, the hub drops a slow consumer, or the key is revoked.
func (h *eventsHandler) handle(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.hub.subscribe()
	if !ok {
		adminhttp.WriteErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many SSE connections")
		return
	}
	defer h.hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // reverse proxy: do not buffer this stream
	w.WriteHeader(http.StatusOK)
	sw := newSSEWriter(w, h.hub.cfg.writeWindow)

	// Initial full-roster snapshot before any deltas (design 19 §4.5). Sent
	// BEFORE the drain loop, so any delta queued meanwhile lands after it; the
	// client converges (snapshot is authoritative full state, deltas idempotent).
	if rows, err := h.hub.roster(r.Context()); err == nil {
		if data, e := json.Marshal(map[string]any{"devices": rows}); e == nil {
			if sw.event("snapshot", "", data) != nil {
				return
			}
		}
	}

	token := bearerToken(r)
	pingT := time.NewTicker(h.hub.cfg.ping)
	defer pingT.Stop()
	reauthT := time.NewTicker(h.hub.cfg.reauth)
	defer reauthT.Stop()

	for {
		select {
		case <-r.Context().Done():
			return // client disconnected
		case <-h.hub.life.Done():
			return // server shutting down
		case <-sub.done:
			return // hub dropped us (mailbox overflow)
		case f := <-sub.ch:
			if sw.event(f.name, f.id, f.data) != nil {
				return
			}
		case <-pingT.C:
			if sw.ping() != nil {
				return
			}
		case <-reauthT.C:
			// The long-lived stream is the one path that would outlive a key
			// revocation (operator auth is per-request, no cache). Re-validate;
			// on row-gone / disabled_at set the key authenticates as absent
			// (SEC-M1) → terminal event: error → the client tears down (close +
			// session.invalidate, D19.7) and lands on login within ADMIN_SSE_REAUTH.
			if _, valid, err := h.hub.authenticate(r.Context(), token); err != nil || !valid {
				_ = sw.event("error", "", []byte(`{"code":"revoked"}`))
				return
			}
		}
	}
}

// bearerToken extracts the raw token from "Authorization: Bearer <t>" for the
// in-stream re-auth. The Auth middleware already validated it at connect; this
// re-reads it to re-check revocation. Returns "" when absent/malformed (→ the
// re-auth lookup fails closed and the stream ends).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(h) <= len(pfx) || !strings.EqualFold(h[:len(pfx)], pfx) {
		return ""
	}
	return strings.TrimSpace(h[len(pfx):])
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
