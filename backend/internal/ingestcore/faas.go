package ingestcore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/open-picpak/backend/internal/faasstore"
)

// FaaS ingest arms (A24 §4.3 / A35 §3). The ingest frame path fronts the device GET /frame and the
// external webhook POST, and reaches the faascore backend by DIRECT in-process method call (the
// render.sock/webhook UDS transport is gone, A35). ingest still carries NO secret and drives NO worker
// inline (D24.1) — it relays the packed frame faascore returns and layers Doc 20's OTA signal (D24.10).

// FrameBackend is the in-process faascore seam the ingest frame path calls (design 35 §3). It replaces
// the POST /render + POST /webhook HTTP-over-UDS transport with two direct method calls;
// faascore.Supervisor satisfies it. Render ALWAYS returns a valid 30000-byte frame + wake/status/stale;
// Fanout launches the webhook fan-out (the token has already been validated by the ingest handler).
type FrameBackend interface {
	Render(ctx context.Context, serial, channel, trigger, now string, force bool) (packed []byte, status string, stale bool, wake int)
	Fanout(ctx context.Context, name string, payload json.RawMessage) error
}

// webhookBodyMax caps the inbound webhook body handed on as the trigger payload.
const webhookBodyMax = 64 << 10

// faasArmed reports whether the /frame + webhook arms are live (BACKEND_FAAS_FRAME_ENABLED, design §3).
// Unset (default) → the arms 404 (route reads absent), the pausability-safe posture RENDER_SOCK-unset
// used to carry.
func (s *Server) faasArmed() bool { return s.frameEnabled }

// faasRetryWakeFromEnv parses FAAS_RETRY_WAKE (seconds); default 300 — short, so the panel recovers
// promptly once the supervisor is back (policy=data, env only).
func faasRetryWakeFromEnv() int {
	if v := os.Getenv("FAAS_RETRY_WAKE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("FAAS_RETRY_WAKE %q invalid, using 300", v)
	}
	return 300
}

// frameResult is the resolved frame + the device-facing wake/status headers.
type frameResult struct {
	packed []byte
	wake   int
	status string // ok | stale | error | unavailable
	stale  string // "0" | "1"
}

func (s *Server) unavailableResult() frameResult {
	return frameResult{packed: unavailableFrame, wake: s.faasRetryWake, status: "unavailable", stale: "1"}
}

// handleFrame is the device frame path. It persists the telemetry/log piggyback exactly as /pp (best-
// effort — the frame is the deliverable), resolves the Server-side channel (D20.5), fetches the packed
// frame from the supervisor (unavailable frame on outage, T13), and layers Doc 20's OTA signal (D24.10).
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serial := first(q.Get("sn"), q.Get("id")) // sn wins; legacy id (MAC-suffix) is the fallback (main.go:127)
	if serial == "" {
		http.NotFound(w, r) // a fetch with no identity is noise
		return
	}
	ctx := r.Context()

	// Telemetry/log ride-along (D24 §4.3): persist it the SAME way /pp does, but BEST-EFFORT — the frame
	// is the device's primary need, so a telemetry-write failure is logged and never blocks the frame (the
	// FW re-pushes on /pp next wake; withholding the log-ack re-drives the log delta too). Only record a
	// point when the FW actually sent telemetry/diag keys — a bare frame fetch writes no near-empty row.
	var ackOff int64
	var acked bool
	if hasExpectedKey(q) {
		var err error
		if ackOff, acked, err = s.ingest(ctx, r, q, serial); err != nil {
			log.Printf("frame ingest %s: %v", serial, err) // best-effort; do not fail the frame
			acked = false
		}
	}

	fr := s.requestFrame(ctx, serial, s.deviceChannel(ctx, serial))

	// K11 header order: the log-ack rides FIRST (set only on a committed, acked push), THEN the OTA signal
	// (fail-open — must not pre-empt the ack). Both before the body write.
	if acked {
		w.Header().Set("X-Log-Ack-Offset", strconv.FormatInt(ackOff, 10))
	}
	w.Header().Set("X-Next-Wake-Seconds", strconv.Itoa(fr.wake))
	w.Header().Set("X-Picpak-Status", fr.status)
	w.Header().Set("X-Picpak-Stale", fr.stale)
	s.injectOTASignal(ctx, w, serial) // OTA half assembled where the ticket key lives (D24.10) — reused, no re-inline
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(fr.packed)))
	_, _ = w.Write(fr.packed)
}

// requestFrame calls the faascore backend in-process (design 35 §3, was POST /render over render.sock).
// faascore ALWAYS returns a valid 30000-byte body (last-good/error frame on failure, D24.6) — so ingest
// only synthesises the unavailable frame when the backend is absent or the body is not frame-shaped (T13,
// defense: an in-process call cannot fail the way a socket connect did, but a wrong length still must
// never reach the panel).
func (s *Server) requestFrame(ctx context.Context, serial, channel string) frameResult {
	if s.frame == nil {
		return s.unavailableResult() // frame arm enabled but no backend wired — misconfig, fail safe
	}
	packed, status, stale, wake := s.frame.Render(ctx, serial, channel, "render", "", false)
	if len(packed) != packedFrameSize {
		log.Printf("faas render %s: non-frame body %d bytes", serial, len(packed))
		return s.unavailableResult() // a non-frame body must never reach the panel (T13)
	}
	if wake <= 0 {
		wake = s.faasRetryWake
	}
	if status == "" {
		status = "ok"
	}
	staleStr := "0"
	if stale {
		staleStr = "1"
	}
	return frameResult{packed: packed, wake: wake, status: status, stale: staleStr}
}

// handleWebhook is the /faas/hook/{name} POST arm. It looks up the named function, verifies it is an
// enabled webhook trigger, and constant-time-compares sha256(presented token) against the stored hash
// (D24.13). Unknown/mismatched → 404/401 with no run. On a valid token it hands the fan-out to the
// supervisor (the POST body as the trigger payload) and replies 202 — the device picks up the new frame
// on its next /frame. The token check lives HERE (ingest holds the DB + the hash reader), not in the
// supervisor; the supervisor endpoint is an internal UDS the token-check gates access to.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	fn, err := faasstore.LoadFunctionByName(ctx, s.pool, name)
	if err != nil || fn.TriggerType != faasstore.TriggerWebhook || !fn.Enabled {
		http.NotFound(w, r) // unknown / not-a-webhook / disabled — a probe learns nothing (uniform 404)
		return
	}
	presented := webhookToken(r)
	sha, err := faasstore.WebhookTokenSHA(ctx, s.pool, fn.ID)
	if err != nil || presented == "" {
		http.Error(w, "", http.StatusUnauthorized) // no token configured / none presented → 401, no run
		return
	}
	sum := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(sum[:], sha) != 1 {
		http.Error(w, "", http.StatusUnauthorized) // constant-time compare (D24.13)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, webhookBodyMax))
	if err := s.triggerWebhook(ctx, fn.Name, body); err != nil {
		log.Printf("faas webhook %s: %v", fn.Name, err)
		http.Error(w, "", http.StatusServiceUnavailable) // supervisor outage → 503, external caller retries
		return
	}
	w.WriteHeader(http.StatusAccepted) // 202 — fan-out is accepted; the device sees the frame next /frame
}

// webhookToken reads the presented token from the X-Faas-Token header, else the ?token= query param.
func webhookToken(r *http.Request) string {
	if t := r.Header.Get("X-Faas-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

// triggerWebhook hands the fan-out to faascore in-process (design 35 §3, was POST /webhook). The POST
// body becomes the trigger payload: passed through verbatim when it is valid JSON, else wrapped as a
// JSON string so the M4 request faascore builds is always valid JSON. An empty body carries no payload.
func (s *Server) triggerWebhook(ctx context.Context, name string, rawBody []byte) error {
	var payload json.RawMessage
	if len(rawBody) > 0 {
		if json.Valid(rawBody) {
			payload = json.RawMessage(rawBody)
		} else {
			enc, _ := json.Marshal(string(rawBody))
			payload = json.RawMessage(enc)
		}
	}
	if s.frame == nil {
		return fmt.Errorf("frame backend not configured")
	}
	return s.frame.Fanout(ctx, name, payload)
}

// deviceChannel reads a device's operator-owned channel Server-side (D20.5) — never the device header.
// Empty when unknown/absent; the supervisor tolerates an empty channel.
func (s *Server) deviceChannel(ctx context.Context, serial string) string {
	var ch string
	_ = s.pool.QueryRow(ctx, `SELECT channel FROM devices WHERE serial = $1`, serial).Scan(&ch)
	return ch
}
