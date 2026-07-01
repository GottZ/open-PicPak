// Command ingest is the public telemetry/OTA ingest endpoint for the open-picpak fleet.
//
// Wave S5b1 (legacy/M1 path): accepts the legacy token-path request that the Python sink
// served: GET /<TOKEN>/pp?... with X-Picpak-Diag / X-Picpak-Log request headers. It parses the
// query + diag header and writes telemetry + logs rows, auto-registering the device by mac.
// HOTP validation (per-device auth) and the OTA-signal response headers land in later waves
// (S5c / S5d); this stage is the backward-compatible ingest only.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/otaticket"
)

// expectedKeys mirror the legacy sink contract: a request is real telemetry only if the token
// path matches AND at least one of these query keys is present (everything else is internet noise).
var expectedKeys = []string{"v", "p", "bad", "bc", "rr", "usb", "up", "id"}

type server struct {
	pool     *pgxpool.Pool
	token    string      // legacy secret path segment; "" disables the legacy route
	notifier *c2Notifier // C2 long-poll wakeup hub (Design 16)

	// OTA serving (A20). otaServeEnabled gates the whole binary serve (default-off, D20.11); otaKey
	// must be >=32 B to arm it (D20.9). All policy=data — env, never a code constant.
	otaServeEnabled bool
	otaKey          string
	otaTTL          time.Duration
	fwBlobDir       string

	// Log reassembly (A21). logFrameMax is the frame plausibility cap (D21.6) — policy=data, env only.
	logFrameMax int64

	// FaaS frame path (A24 §4.3). faasClient (nil ⇒ arms disabled, default) dials the faas-supervisor
	// render-request seam over RENDER_SOCK (HTTP-over-UDS); faasRetryWake is the wake ingest sets when
	// it serves the unavailable frame (supervisor outage). No secret/DB-render logic lives here (D24.1).
	faasClient    *http.Client
	faasRetryWake int
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// assemble from the compose POSTGRES_* vars
		dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			env("POSTGRES_USER", "picpak"), os.Getenv("POSTGRES_PASSWORD"),
			env("PGHOST", "timescaledb:5432"), env("POSTGRES_DB", "picpak"))
	}
	addr := env("LISTEN_ADDR", ":8080")
	token := os.Getenv("INGEST_TOKEN") // legacy path token; keep out of code (air-gap)

	// OTA serving config (A20) — policy=data, env only. Default-off (D20.11); weak/empty key 503-
	// disables the binary serve at request time (D20.9). FW_BLOB_DIR is the read-only blob mount (§4.7).
	otaServeEnabled := os.Getenv("OTA_SERVE_ENABLED") == "true"
	otaKey := os.Getenv("OTA_TICKET_KEY")
	otaTTL := otaTicketTTL()
	fwBlobDir := env("FW_BLOB_DIR", "/fwblobs")
	logFrameMax := logFrameMaxFromEnv() // A21 frame plausibility cap (D21.6)

	// FaaS frame path (A24 §4.3) — policy=data, env only. RENDER_SOCK unset ⇒ arms disabled (default,
	// pausability-safe): /frame + /faas/hook read as absent (404). FAAS_RETRY_WAKE is the wake ingest
	// sets on the unavailable frame (supervisor outage) — a short retry so the panel recovers fast.
	faasClient := newFaasClient(os.Getenv("RENDER_SOCK"))
	faasRetryWake := faasRetryWakeFromEnv()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("db pool: %v", err)
	}
	defer pool.Close()
	// fail fast if the DB is unreachable at boot (surfaces config drift, not a silent half-up service)
	if err := pingWithRetry(ctx, pool, 10, time.Second); err != nil {
		log.Fatalf("db unreachable: %v", err)
	}

	s := &server{pool: pool, token: token, notifier: newC2Notifier(),
		otaServeEnabled: otaServeEnabled, otaKey: otaKey, otaTTL: otaTTL, fwBlobDir: fwBlobDir,
		logFrameMax: logFrameMax, faasClient: faasClient, faasRetryWake: faasRetryWake}
	// One LISTEN connection feeds the C2 long-poll wakeup hub for the whole fleet (Design 16).
	go s.notifier.listenLoop(ctx, pool)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", s.handle)

	log.Printf("ingest listening on %s (legacy token route %s, OTA serve %s, FaaS frame path %s)",
		addr, routeState(token), otaServeState(otaServeEnabled, otaKey), faasState(faasClient))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	// secret-path contract: GET /<token>/{pp|c2}?...  (token kept in env, never in code).
	// One shared path token gates both endpoints for now; per-device HOTP supersedes it (wave 3d).
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if s.token == "" || len(parts) < 2 || parts[0] != s.token {
		http.NotFound(w, r) // 404 == noise (mirrors the legacy sink)
		return
	}
	sub := ""
	if len(parts) >= 3 {
		sub = parts[2]
	}
	switch {
	case parts[1] == "pp" && sub == "" && r.Method == http.MethodGet:
		s.handleIngest(w, r)
	case parts[1] == "c2" && sub == "" && r.Method == http.MethodGet:
		s.handleC2(w, r)
	case parts[1] == "c2" && sub == "challenge" && r.Method == http.MethodGet:
		s.handleC2Challenge(w, r) // Doc 15 re-key handshake
	case parts[1] == "c2" && sub == "rekey" && r.Method == http.MethodPost:
		s.handleC2Rekey(w, r)
	case parts[1] == "firmware.bin" && sub == "" && r.Method == http.MethodGet:
		s.handleFirmware(w, r) // A20 OTA binary serve — gated default-off + ticket (D20.11/D20.9/D20.8)
	case s.faasArmed() && parts[1] == "frame" && sub == "" && r.Method == http.MethodGet:
		s.handleFrame(w, r) // A24 device frame path (§4.3) — proxies the supervisor, layers the OTA signal
	case s.faasArmed() && parts[1] == "faas" && sub == "hook" && r.Method == http.MethodPost:
		name := "" // /<token>/faas/hook/<name>: the per-function webhook token gates it (§4.3/D24.13)
		if len(parts) >= 4 {
			name = parts[3]
		}
		s.handleWebhook(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !hasExpectedKey(q) {
		http.NotFound(w, r)
		return
	}

	serial := first(q.Get("sn"), q.Get("id")) // sn (D8) wins; legacy id (MAC-suffix) is the fallback
	if serial == "" {
		http.NotFound(w, r) // a point with no identity is noise
		return
	}

	ackOff, acked, err := s.ingest(r.Context(), r, q, serial)
	if err != nil {
		log.Printf("ingest %s: %v", serial, err)
		http.Error(w, "", http.StatusInternalServerError) // FW retries next wake
		return
	}
	// masterplan K11: the log ack rides this post-commit /pp path FIRST, then the OTA signal. The header
	// is set only on a committed, acked push (durable-before-ack, D21.2/Doc 04 §5.4); a COMMIT failure
	// returns err above and the header is never set (T2). injectOTASignal's fail-open return MUST NOT
	// pre-empt this — else a committed log delta gets no ack, the FW re-pushes forever and overruns its
	// ring, dropping real log lines (A21 O5).
	if acked {
		w.Header().Set("X-Log-Ack-Offset", strconv.FormatInt(ackOff, 10))
	}
	s.injectOTASignal(r.Context(), w, serial) // OTA signal, fail-open (D20.8) — after commit, before body
	_, _ = w.Write([]byte("ok"))
}

// ingest writes the telemetry row and, when the push carries a log ride-along, reassembles it inside the
// SAME tx (A21). It returns the durable log-ack high-water + whether the push was acked, so handleIngest
// can echo X-Log-Ack-Offset post-commit (D21.2). acked is false on a telemetry-only push or a skipped bad
// frame (D21.6); err is non-nil only on a real DB/tx failure (the caller 500s and the FW retries).
func (s *server) ingest(ctx context.Context, r *http.Request, q map[string][]string, serial string) (ackOff int64, acked bool, err error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	d := parseDiag(r.Header.Get("X-Picpak-Diag"))
	srcIP := clientIP(r)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// auto-register (M1): create/refresh the device row; NEVER touch channel/secret here
	// (single admin-writer on channel; secret only via the provisioning path).
	mac := get("id")
	if _, err := tx.Exec(ctx,
		`INSERT INTO devices (serial, mac, last_seen) VALUES ($1, NULLIF($2,''), now())
		 ON CONFLICT (serial) DO UPDATE SET last_seen = now(),
		   mac = COALESCE(devices.mac, EXCLUDED.mac)`,
		serial, mac); err != nil {
		return 0, false, fmt.Errorf("device upsert: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO telemetry (time, serial, batt_mv, batt_pct, bad_boots, boot_count, reset_reason,
		   usb, uptime_ms, running_ver, channel,
		   diag_run_part, diag_runstate, diag_o0, diag_o1, diag_inv, diag_boots, diag_rr, diag_ota_rr,
		   diag_mv, diag_mv_err, diag_stage, src_ip)
		 VALUES (now(), $1, $2,$3,$4,$5,$6, $7,$8,$9,$10,
		   $11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21, $22)`,
		serial, ni(get("v")), ni(get("p")), ni(get("bad")), ni(get("bc")), ntext(get("rr")),
		nbool(get("usb")), ni(get("up")), ntext(get("fw")), ntext(get("ch")),
		d["run"], ni(d["runstate"]), ni(d["o0"]), ni(d["o1"]), ntext(d["inv"]), ni(d["boots"]),
		ni(d["rr"]), ni(d["ota_rr"]), ni(d["mv"]), ni(d["mv_err"]), ntext(d["stage"]), nIP(srcIP)); err != nil {
		return 0, false, fmt.Errorf("telemetry insert: %w", err)
	}

	// X-Picpak-Log ride-along: reassemble the ring tail (A21) — offset-ack / gap / suspect / seq /
	// idempotent fragment, in this SAME tx so a bad log frame can never lose the telemetry (D21.6). The
	// ack value is read-your-write inside the tx and echoed by the caller only after COMMIT (D21.2).
	if logHdr := r.Header.Get("X-Picpak-Log"); logHdr != "" {
		if lf, ok := parseLogFrame(q, r.Header.Get("X-Picpak-Log-Frame")); ok {
			ackOff, acked, err = reassembleLog(ctx, tx, serial, lf, logHdr, s.logFrameMax)
			if err != nil {
				return 0, false, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err // COMMIT failed → withhold the ack (T2); telemetry not durable, FW retries
	}
	return ackOff, acked, nil
}

// parseDiag splits the X-Picpak-Diag single-line header ("k=v k=v ...") into a map. Tolerant:
// unknown keys are kept, missing keys are simply absent. Matches the firmware's space-separated format.
func parseDiag(s string) map[string]string {
	m := map[string]string{}
	for _, tok := range strings.Fields(s) {
		if k, v, ok := strings.Cut(tok, "="); ok {
			m[k] = v
		}
	}
	return m
}

// Small helpers: return typed values or nil so absent/unparsable fields store as SQL NULL.

func ni(s string) any { // integer or NULL
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return nil
}
func nbool(s string) any { // "1"/"0" or NULL
	switch s {
	case "1":
		return true
	case "0":
		return false
	}
	return nil
}
func ntext(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nIP(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func hasExpectedKey(q map[string][]string) bool {
	for _, k := range expectedKeys {
		if _, ok := q[k]; ok {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func routeState(token string) string {
	if token == "" {
		return "DISABLED"
	}
	return "enabled"
}

// otaServeState describes the firmware.bin serve posture for the boot log (D20.9/D20.11): off by
// default, explicitly flagged when serving is on but the key is too weak to sign with.
func otaServeState(enabled bool, key string) string {
	switch {
	case !enabled:
		return "DISABLED (default-off)"
	case !otaticket.KeyStrong(key):
		return "DISABLED (ticket key absent/weak)" // D20.9
	default:
		return "enabled"
	}
}

// otaTicketTTL parses OTA_TICKET_TTL (a Go duration, e.g. "120s"); default 120s (§4.5) — long enough
// for /pp→compare→firmware.bin, short enough to bound replay.
func otaTicketTTL() time.Duration {
	if v := os.Getenv("OTA_TICKET_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("OTA_TICKET_TTL %q invalid, using 120s", v)
	}
	return 120 * time.Second
}

func pingWithRetry(ctx context.Context, pool *pgxpool.Pool, tries int, wait time.Duration) error {
	var err error
	for i := 0; i < tries; i++ {
		if err = pool.Ping(ctx); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return fmt.Errorf("ping failed after %d tries: %w", tries, err)
}
