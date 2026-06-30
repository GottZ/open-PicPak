// Command admin is the operator control plane for the open-picpak fleet: bearer-authenticated CRUD,
// command enqueue, rollout/secret/FaaS management. It runs in a separate process and address space
// from the public ingest binary (the secret/rollout write path must never sit in the public parser).
//
// Run with no arguments to serve the admin API. The operator-key management subcommands bootstrap and
// revoke bearer keys out-of-band (no chicken-and-egg HTTP auth needed for the first key):
//
//	admin create-operator -label <name> [-admin]   # mint a key, print the token ONCE
//	admin list-operators                           # list keys (never the token/hash)
//	admin disable-operator -id <n>                 # soft-revoke a key (SEC-M1)
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/operator"
	"github.com/open-picpak/backend/internal/sealbox"
	"github.com/open-picpak/backend/internal/secrets"
	"github.com/open-picpak/backend/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create-operator", "list-operators", "disable-operator":
			os.Exit(runOperatorCLI(os.Args[1], os.Args[2:]))
		case "-secret-decrypt", "secret-decrypt":
			os.Exit(runSecretDecrypt(os.Stdin, os.Stdout, os.Stderr, stdoutIsTTY()))
		}
	}
	runServer()
}

// openPool assembles the DSN (env DATABASE_URL, else the compose POSTGRES_* vars — same shape as
// ingest) and returns a pool that has passed a fail-fast boot ping (surfaces config drift, not a
// silent half-up service).
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			env("POSTGRES_USER", "picpak"), os.Getenv("POSTGRES_PASSWORD"),
			env("PGHOST", "timescaledb:5432"), env("POSTGRES_DB", "picpak"))
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pingWithRetry(ctx, pool, 10, time.Second); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func runServer() {
	// SEC-M3: the operator plane must hold a valid master key or refuse to boot — fail-closed,
	// symmetric to the public ingest token gate. Checked before the DB connect so a missing/short/
	// invalid SECRETS_KEY is a deterministic boot failure (not masked by a DB error), and so an admin
	// without a usable key never serves even the device routes. The Box is the master key for the
	// secret routes and the boot rotation sweep.
	box, err := sealbox.FromEnv()
	if err != nil {
		log.Fatalf("secrets: %v", err)
	}

	ctx := context.Background()
	pool, err := openPool(ctx)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	// GAP-M3: boot-only rotation sweep. With SECRETS_KEY_PREV set, re-seal prev-key rows under the
	// current key. A stranded row is a brick signal — the service still comes up to serve the rest,
	// but SECRETS_KEY_PREV must stay set until the warning clears (never drop prev while rows remain).
	if box.HasPrev() {
		reSealed, stranded, serr := secrets.Sweep(ctx, pool, box)
		log.Printf("secrets: rotation sweep re-sealed %d row(s)", reSealed)
		if serr != nil {
			log.Printf("secrets: ROTATION INCOMPLETE — keep SECRETS_KEY_PREV set; %d secret(s) not re-sealed: %v",
				len(stranded), stranded)
		}
	}

	// Operator zone binds to loopback/VPN by default — NEVER the public ingest interface. TLS
	// terminates at the reverse proxy, as with ingest.
	addr := env("ADMIN_ADDR", "127.0.0.1:8081")

	dh := deviceHandlers{pool: pool}
	ch := commandHandlers{pool: pool}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	// whoami is auth-gated (any valid key) — identity + capability, the SPA's read-only-vs-admin probe.
	mux.Handle("GET /api/whoami", adminhttp.Auth(pool)(http.HandlerFunc(whoami)))
	// device registry: reads are auth-gated (any key); mutations → requireAdmin.
	mux.Handle("GET /api/devices", adminhttp.Auth(pool)(http.HandlerFunc(dh.list)))
	mux.Handle("GET /api/devices/{serial}", adminhttp.Auth(pool)(http.HandlerFunc(dh.get)))
	mux.Handle("POST /api/devices", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(dh.register))))
	mux.Handle("PATCH /api/devices/{serial}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(dh.patch))))
	mux.Handle("DELETE /api/devices/{serial}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(dh.delete))))
	// command enqueue (RCE-capable) → requireAdmin, attributed to the operator key.
	mux.Handle("POST /api/command", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(ch.enqueue))))
	mux.Handle("POST /api/devices/{serial}/command", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(ch.enqueue))))
	// secrets KV (write-only; value never echoed) → requireAdmin. The break-glass decrypt path is the
	// out-of-band `admin -secret-decrypt` subcommand, not an HTTP route.
	sh := secretHandlers{pool: pool, box: box}
	mux.Handle("PUT /api/secrets/{name}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(sh.put))))
	mux.Handle("GET /api/secrets", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(sh.list))))
	mux.Handle("GET /api/secrets/{name}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(sh.get))))
	mux.Handle("DELETE /api/secrets/{name}", adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(sh.del))))

	// SSE scaffold (D19.7): live roster/telemetry/log stream. auth-gated (any valid
	// key, O1) — the generic events mirror the GET read routes. A feature channel
	// that pushes admin-only data must additionally re-auth on is_admin (§4.5 hook).
	eh := newEventsHandler(ctx, pool)
	mux.Handle("GET /api/events", adminhttp.Auth(pool)(http.HandlerFunc(eh.handle)))

	// SPA catch-all (D19.1): the embedded Svelte admin UI on "/". Registered LAST —
	// stdlib ServeMux longest-pattern precedence keeps every "/api/..." and "/healthz"
	// route ahead of "/", so a wrong-method hit on a known API path stays its own 4xx,
	// not the SPA. A binary built without the bun frontend stage serves a 503 hint here
	// while all /api routes stay functional (D19.3).
	mux.Handle("/", web.Handler())

	handler := adminhttp.WithRequestID(mux)
	log.Printf("admin listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func whoami(w http.ResponseWriter, r *http.Request) {
	op, _ := adminhttp.Operator(r.Context())
	adminhttp.WriteOK(w, r, whoamiFields(op))
}

// whoamiFields is the GET /api/whoami payload (sans the envelope's success).
// The field name is **is_admin** (snake_case) — the SPA's read-only badge
// (design 19 D19.6) derives off it; a rename breaks T9, not silently the UI.
func whoamiFields(op operator.AuthResult) map[string]any {
	return map[string]any{"key_id": op.KeyID, "is_admin": op.IsAdmin, "label": op.Label}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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
