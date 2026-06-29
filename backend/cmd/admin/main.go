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
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create-operator", "list-operators", "disable-operator":
			os.Exit(runOperatorCLI(os.Args[1], os.Args[2:]))
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
	ctx := context.Background()
	pool, err := openPool(ctx)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	// Operator zone binds to loopback/VPN by default — NEVER the public ingest interface. TLS
	// terminates at the reverse proxy, as with ingest.
	addr := env("ADMIN_ADDR", "127.0.0.1:8081")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	// whoami is auth-gated (any valid key) — identity + capability, the SPA's read-only-vs-admin probe.
	mux.Handle("GET /api/whoami", adminhttp.Auth(pool)(http.HandlerFunc(whoami)))

	handler := adminhttp.WithRequestID(mux)
	log.Printf("admin listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func whoami(w http.ResponseWriter, r *http.Request) {
	op, _ := adminhttp.Operator(r.Context())
	adminhttp.WriteOK(w, r, map[string]any{"key_id": op.KeyID, "is_admin": op.IsAdmin, "label": op.Label})
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
