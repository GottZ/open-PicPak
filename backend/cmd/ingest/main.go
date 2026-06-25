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
)

// expectedKeys mirror the legacy sink contract: a request is real telemetry only if the token
// path matches AND at least one of these query keys is present (everything else is internet noise).
var expectedKeys = []string{"v", "p", "bad", "bc", "rr", "usb", "up", "id"}

type server struct {
	pool  *pgxpool.Pool
	token string // legacy secret path segment; "" disables the legacy route
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

	s := &server{pool: pool, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", s.handle)

	log.Printf("ingest listening on %s (legacy token route %s)", addr, routeState(token))
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	// legacy contract: GET /<token>/pp?...  (token kept in env, never in code)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method != http.MethodGet || s.token == "" || len(parts) < 2 || parts[0] != s.token || parts[1] != "pp" {
		http.NotFound(w, r) // 404 == noise (mirrors the legacy sink)
		return
	}
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

	if err := s.ingest(r.Context(), r, q, serial); err != nil {
		log.Printf("ingest %s: %v", serial, err)
		http.Error(w, "", http.StatusInternalServerError) // FW retries next wake
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *server) ingest(ctx context.Context, r *http.Request, q map[string][]string, serial string) error {
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
		return err
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
		return fmt.Errorf("device upsert: %w", err)
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
		return fmt.Errorf("telemetry insert: %w", err)
	}

	// X-Picpak-Log: store the ring tail. Full reassembly + offset-ack + gap/suspect is wave S5f;
	// here we just persist the payload with the device-claimed offset (NULL if absent).
	if logHdr := r.Header.Get("X-Picpak-Log"); logHdr != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO logs (time, serial, boot_count, offset_start, payload, source)
			 VALUES (now(), $1, $2, $3, $4, 'telemetry')`,
			serial, ni(get("bc")), ni(get("off")), logHdr); err != nil {
			return fmt.Errorf("log insert: %w", err)
		}
	}
	return tx.Commit(ctx)
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
