// Command backend is the ONE open-picpak backend process (A35 / DECISIONS §A35): the operator control
// plane (bearer-authenticated CRUD, command enqueue, rollout/secret/FaaS management, SPA), the public
// device ingest (telemetry/OTA/C2/frame), and the faas-supervisor logic (scheduler, render, test-render/
// preview) all in ONE process, ONE pool, ONE mux, behind ONE origin. The M5 address-space split is
// retired by decision (design 35 §5); the defence is now handler discipline + per-request recover, not a
// process boundary. faas-worker + faas-egress stay separate (sandbox), reached as a client (m4.sock /
// egress-ctl.sock, byte-identical).
//
// Run with no arguments to serve. The operator-key management subcommands bootstrap and revoke bearer
// keys out-of-band (no chicken-and-egg HTTP auth needed for the first key):
//
//	backend create-operator -label <name> [-admin]   # mint a key, print the token ONCE
//	backend list-operators                           # list keys (never the token/hash)
//	backend disable-operator -id <n>                 # soft-revoke a key (SEC-M1)
//	backend rotate-operator -id <n> [-expires-in d]  # mint a replacement, retire the old on a grace window
//	backend create-user -username <name> [-admin]    # create a human admin_users account (password prompt)
//	backend create-api-token -label <name> ...       # mint a machine api_token, print the token ONCE
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/faascore"
	"github.com/open-picpak/backend/internal/ingestcore"
	"github.com/open-picpak/backend/internal/sealbox"
	"github.com/open-picpak/backend/internal/secrets"
	"github.com/open-picpak/backend/internal/templatestore"
	"github.com/open-picpak/backend/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "create-operator", "list-operators", "disable-operator", "rotate-operator":
			os.Exit(runOperatorCLI(os.Args[1], os.Args[2:]))
		case "create-user":
			os.Exit(runUserCLI(os.Args[1], os.Args[2:]))
		case "create-api-token":
			os.Exit(runTokenCLI(os.Args[1], os.Args[2:]))
		case "-secret-decrypt", "secret-decrypt":
			os.Exit(runSecretDecrypt(os.Stdin, os.Stdout, os.Stderr, stdoutIsTTY()))
		}
	}
	runServer()
}

// openPool assembles the DSN (env DATABASE_URL, else the compose POSTGRES_* vars) and returns a pool that
// has passed a fail-fast boot ping (surfaces config drift, not a silent half-up service). MaxConns is
// dimensioned for the MERGED union (design 35 §6): two permanent LISTEN holders (c2_cmd, and — default-
// off — playlist_changed) plus C2-longpoll @ fleet scale + admin + render. Startup default is
// max(16, 4×NumCPU), overridable via BACKEND_MAX_CONNS; Postgres max_connections must be sized above it.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
			env("POSTGRES_USER", "picpak"), os.Getenv("POSTGRES_PASSWORD"),
			env("PGHOST", "timescaledb:5432"), env("POSTGRES_DB", "picpak"))
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = int32(maxConns())
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pingWithRetry(ctx, pool, 10, time.Second); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// maxConns is the merged pool ceiling (design 35 §6): BACKEND_MAX_CONNS if a positive override is set,
// else max(16, 4×NumCPU). Two of these are permanently held by the LISTEN loops (c2notify + the default-
// off playlist warmer); the rest carry the vereinigte request load.
func maxConns() int {
	if v := os.Getenv("BACKEND_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("BACKEND_MAX_CONNS %q invalid, using default", v)
	}
	if n := 4 * runtime.NumCPU(); n > 16 {
		return n
	}
	return 16
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

	// Prefab template catalog (A30 W1): idempotently upsert the embedded builtins into `templates`.
	// The IS-DISTINCT-FROM guard makes a restart on an unchanged catalog a true no-op (no version
	// bump), and the whole loop runs under an advisory lock so concurrent replica boots serialise.
	// Fail-open: a seed error still serves the rest of the plane (parity the secrets sweep) — a
	// broken embedded catalog would already have panicked at build/embed time.
	if res, terr := templatestore.SeedBuiltins(ctx, pool); terr != nil {
		log.Printf("templatestore: WARN builtin seed failed, prefab catalog may be stale: %v", terr)
	} else {
		log.Printf("templatestore: builtin seed ok (inserted=%d updated=%d unchanged=%d)",
			res.Inserted, res.Updated, res.Unchanged)
	}

	// The merged backend binds ONE public listener (BACKEND_ADDR, wildcard = OriginPublic — device +
	// admin + SPA behind both host labels) plus a distinct loopback break-glass listener
	// (BACKEND_LOOPBACK_ADDR); the W7 operator_key fallback rides the loopback origin (design 35 §4). TLS
	// terminates at the reverse proxy.
	addr := env("BACKEND_ADDR", ":8080")

	// FaaS in-process supervisor (design 35 §2/§3): scheduler + render/webhook/test-render seams, reached
	// by DIRECT method call (no render.sock/test.sock). It shares the ONE pool + Box (M5 collapsed).
	sup := faascore.New(pool, box)
	sup.Start(ctx) // scheduler + (default-off) playlist warmer LISTEN goroutines (§6)

	// Arm flags (design 35 §3) replace the old socket-presence gates:
	//   BACKEND_FAAS_FRAME_ENABLED — false (default) ⇒ /frame + /faas/hook 404 ("absent"), was RENDER_SOCK.
	//   BACKEND_TEST_RENDER_ENABLED — false (default) ⇒ test-run 503 + preview degrades, was FAAS_TEST_SOCK.
	faasFrameEnabled := os.Getenv("BACKEND_FAAS_FRAME_ENABLED") == "true"
	testRenderEnabled := faascore.TestRenderArmed()

	dh := deviceHandlers{pool: pool, purgeTelemetry: envBool("ADMIN_DELETE_PURGES_TELEMETRY", false)}
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

	// OTA serving/rollout management (A20): firmware register (admin, blob re-hash), channel default,
	// rollout CRUD, and the authoritative GET /api/resolve. Reads auth-gated, mutations requireAdmin.
	// FW_BLOB_DIR is mounted :rw here (register writes), :ro in ingest (serve streams) — §4.7.
	registerOTARoutes(mux, pool, env("FW_BLOB_DIR", "/fwblobs"))

	// Log viewer read surface (A21): keyset query + serials picker + gapless reconstruct, all auth-gated
	// (read-only, D21.7). Plus the minimal fragment-prune ticker (§4.7) — the fragment table has no
	// retention of its own, so it would grow unbounded from reassembly without this.
	registerLogRoutes(mux, pool)
	startFragmentPrune(ctx, pool)

	// Human session-login surface (A28 W3, design §4.3): POST /api/session (login, no auth) and DELETE
	// /api/session (logout). The Basic carrier (delta-4) needs no wiring here — it lives inside
	// adminhttp.Auth, already on every gated route. Plus the expired-session prune ticker (§3.2), the
	// startFragmentPrune analogue for admin_sessions.
	registerAuthRoutes(mux, pool)
	startSessionPrune(ctx, pool)

	// Machine api-token management surface (A28 W6, design §4.5): mint / list / soft-revoke of
	// api_tokens, all IsAdmin-gated. HTTP minting for an already-authenticated admin; the CLI
	// (create-api-token) bootstraps the first token out-of-band. Single wiring source (registerTokenRoutes)
	// so this test-gated mount and main.go never drift. Registered before the SPA catch-all below.
	registerTokenRoutes(mux, pool)

	// Image CRUD surface (A28 W5, design §4.2): upload (content-addressed, idempotent) / keyset list /
	// meta + raw byte serve (B5 mime-hardened) / delete + audit. Writes gate on image:write, reads on
	// image:read (RequireScope). Blobs live in the content-addressed imgblobs volume (:rw here, supervisor
	// :ro at render, A27/§9). Single wiring source (registerImageRoutes); registered before the SPA
	// catch-all. ADMIN_MAX_IMAGE_BYTES is the compressed-blob cap (Policy=Data, E28.3).
	imgBlobDir := env("IMG_BLOB_DIR", "/imgblobs")
	registerImageRoutes(mux, pool, imgBlobDir, int64(envIntOr("ADMIN_MAX_IMAGE_BYTES", defaultMaxImageBytes)))

	// A27 W6 image maintenance (design §6): the orphan-blob reconcile sweep (runs here — imgblobs is
	// :rw on admin, :ro on the supervisor) and the refcount-driven variant-cache GC. Both are the
	// startFragmentPrune analogue: without them the blob volume and frame_variant_cache grow
	// monotonically at fleet scale. Neither is a created_at TTL (the anti-TTL invariant, §6).
	startOrphanBlobSweep(ctx, pool, imgBlobDir)
	startVariantCacheGC(ctx, pool)

	// Playlist CRUD + device-binding surface (A28 W5b, design 29 §Merge-Punkt A28 (f)/(g), design 27
	// §4.2/§4.3): create/list/get/update/delete a playlist, add/remove/reorder items, and bind/unbind a
	// device to a playlist. Reads gate on image:read, writes on image:write (the SAME media scopes the
	// image surface uses); the device→playlist bind is admin-gated (mirror of the render bind). Mounts on
	// the A27 playliststore + plrender packages and fires NotifyPlaylistChanged on every mutation/bind so
	// the pre-pack warmer can warm variants before devices wake. Single wiring source
	// (registerPlaylistRoutes); registered before the SPA catch-all.
	registerPlaylistRoutes(mux, pool)

	// Telemetry dashboard read surface (A22): the enriched fleet list (the 22→17 seam — adds
	// running_ver/batt/health), the per-device latest+history, and the non-secret SPA config (Grafana/
	// webhook base URLs). All auth-gated (read-only key reaches them); the verdict thresholds + caps are
	// ADMIN_TELEMETRY_* env (Policy=Data, D22.10).
	registerTelemetryRoutes(mux, pool)

	// Berry editor read surface (A23 W1): the capability manifest (the editor's C2 safe-subset catalog,
	// declared DATA + parity-tested against the firmware be_regfunc sites, D23.2). Auth-gated (any valid
	// key — a read-only operator may author/lint a draft; only enqueue is admin-gated). No write path here.
	registerBerryRoutes(mux, pool)

	// FaaS function store (A24 W5): the 7-route function CRUD + the per-device render binding (§4.8).
	// Reads auth-gated (any valid key inspects source/config/bindings); mutations requireAdmin. The
	// webhook token is generated + shown ONCE on create/rotate, its sha256 stored, never echoed (D24.13/K8).
	// The source is foreign code at rest — never eval'd here, only the worker evaluates it (D24.7).
	registerFaasRoutes(mux, pool)

	// Template store (A30 W2): the 5-route template CRUD (prefab + operator-authored blueprints, §4.2).
	// Reads auth-gated (any valid key browses the catalog / loads a template into the editor); mutations
	// requireAdmin — the same gating as the function store. A builtin row is immutable (409); its source
	// of truth is the embedded catalog upserted by SeedBuiltins at startup. The /apply dispatch (which
	// enqueues a device script / mints a runnable function, RCE-equivalent) is a later wave. Single wiring
	// source (registerTemplateRoutes); registered before the SPA catch-all.
	registerTemplateRoutes(mux, pool)
	// Template preview backend (W-A33.5a): GET /api/templates/{id}/preview (durable PNG cache, ETag/304)
	// + builtin warmup under a single fleet-slot budget. Uses the in-process faascore test-render seam
	// (egress-DENIED, EgressAllow=[]); gated by BACKEND_TEST_RENDER_ENABLED (unset ⇒ previews degrade).
	registerTemplatePreviewRoutes(ctx, mux, pool, sup, testRenderEnabled)

	// FaaS editor support (A25 W1): the binding READS A24 left to the editor wave — forward
	// (which function a device renders) + reverse (a function's blast radius, D25.9) + the unbind.
	// Reads auth-gated; the unbind + test-run requireAdmin. test-run drives the in-process faascore
	// test-render arm; BACKEND_TEST_RENDER_ENABLED unset ⇒ the route 503s (pausability-safe, arm dark).
	registerFaasUIRoutes(mux, pool, sup, testRenderEnabled)

	// Web-USB onboarding firmware artifacts (A26 W2): serve the open-picpak CFW manifest + bin parts for the
	// /onboard page's WebSerial flash. Traversal-safe, read-only, same-origin (D26.9). Default off —
	// ADMIN_ONBOARD_FW_DIR unset ⇒ the route 404s a "not configured" notice while flash-from-local + backup
	// still work (pausability-safe, arm dark).
	registerOnboardFWRoutes(mux, pool, env("ADMIN_ONBOARD_FW_DIR", ""))

	// Web-USB onboarding defaults (A35.3b): admin-gated GET /api/onboard/defaults serves the fully
	// tokenized device frame-/C2-URLs so an admin operator does not paste the INGEST_TOKEN by hand. The
	// token is now HTTP-readable to admins (DECISIONS §A35-W3b: strictly weaker than an admin RCE enqueue).
	registerOnboardDefaultsRoutes(mux, pool)

	// SSE scaffold (D19.7): live roster/telemetry/log stream. auth-gated (any valid
	// key, O1) — the generic events mirror the GET read routes. A feature channel
	// that pushes admin-only data must additionally re-auth on is_admin (§4.5 hook).
	eh := newEventsHandler(ctx, pool)
	mux.Handle("GET /api/events", adminhttp.Auth(pool)(http.HandlerFunc(eh.handle)))

	// Root catch-all (design 35 §2): token-dispatch BEFORE the SPA. The ingest Server owns the device
	// secret-token paths (/<token>/{pp|c2|frame|firmware.bin|faas/hook}); a token-prefixed hit dispatches
	// to it (an unknown subpath stays a 404 — the noise-404 semantic the legacy sink carried, preserved),
	// and every non-token "/" hit falls through to the embedded Svelte SPA. Registered LAST — stdlib
	// ServeMux longest-pattern precedence keeps every "/api/..." and "/healthz" route ahead of "/", so a
	// wrong-method hit on a known API path stays its own 4xx. A binary built without the bun frontend
	// stage serves a 503 hint from web.Handler while all /api routes stay functional (D19.3).
	ingestSrv := ingestcore.NewServer(pool, sup, faasFrameEnabled)
	ingestSrv.StartNotifier(ctx) // C2 long-poll LISTEN hub — one dedicated pool conn (§6)
	spa := web.Handler()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if ingestSrv.OwnsPath(r) {
			ingestSrv.Handle(w, r)
			return
		}
		spa.ServeHTTP(w, r)
	})

	// Pre-auth IP rate limit wraps the whole mux (design §4.4): an IP over the limit is 429'd before the
	// mux dispatches, so a credential/login flood is stopped ahead of the argon2 verify it targets, and
	// POST /api/session's brute-force surface is covered without a per-route hook. WithRequestID stays
	// outermost so the 429 still carries an X-Request-ID. The post-auth per-principal brake lives inside
	// adminhttp.Auth, already on every gated route.
	handler := adminhttp.WithRequestID(adminhttp.IPRateLimit(mux))

	// Bind one http.Server per listener spec (design 35 §4, W7-Modell = adminListeners logic unchanged).
	// The BACKEND_ADDR listener is tagged by its own origin; when it is PUBLIC a second loopback listener
	// on BACKEND_LOOPBACK_ADDR is added so the operator_key break-glass + Basic tunnel path stays reachable
	// off the public socket — its OriginLoopback tag is what opens the W7-gated operator_key bearer
	// fallback. When BACKEND_ADDR is itself loopback (dev) one listener already IS the loopback zone, so
	// adminListeners collapses to a single bind. Each server tags every request on it with its listener
	// origin — non-spoofable, from the bind address, never a client header. This origin also drives the
	// ingest telemetry IP reader (adminhttp.RemoteIP, design 35 §4/F5).
	specs := adminListeners(addr, env("BACKEND_LOOPBACK_ADDR", "127.0.0.1:8081"))
	srvCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	servers := make([]*http.Server, 0, len(specs))
	errc := make(chan error, len(specs))
	for _, sp := range specs {
		srv := &http.Server{Addr: sp.addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		srv.BaseContext = adminhttp.ListenerBaseContext(srvCtx, sp.origin)
		servers = append(servers, srv)
		log.Printf("backend listening on %s (%s)", sp.addr, sp.origin)
		go func(s *http.Server) { errc <- s.ListenAndServe() }(srv)
	}
	// The first server to return (a bind failure or a closed listener) tears the others down so the
	// process exits as a unit rather than limping on a half-open control plane. Shutdown drains the
	// still-serving listeners before the fatal exit.
	exitErr := <-errc
	cancel()
	shutCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for _, s := range servers {
		_ = s.Shutdown(shutCtx)
	}
	log.Fatalf("backend server exited: %v", exitErr)
}

// listenerSpec is one socket the admin process binds: its bind address and the non-spoofable origin
// tag every request arriving on it carries (design §4.1). The origin drives the W7 operator_key gate.
type listenerSpec struct {
	addr   string
	origin adminhttp.ListenerOrigin
}

// adminListeners resolves the set of sockets to bind (design §4.1 / §5 B8, W8). The ADMIN_ADDR listener
// is always present, tagged by its own origin. A second loopback listener on loopbackAddr is added ONLY
// when ADMIN_ADDR is PUBLIC — that is the post-flip state where the operator_key break-glass + Basic
// tunnel path must stay reachable off the public socket (OriginLoopback opens the W7-gated fallback).
// When ADMIN_ADDR is itself loopback (dev), or loopbackAddr is empty or identical to it, one listener
// already covers the loopback semantics and no second bind is made.
func adminListeners(addr, loopbackAddr string) []listenerSpec {
	primary := listenerSpec{addr: addr, origin: originForAddr(addr)}
	if primary.origin == adminhttp.OriginLoopback || loopbackAddr == "" || loopbackAddr == addr {
		return []listenerSpec{primary}
	}
	return []listenerSpec{primary, {addr: loopbackAddr, origin: adminhttp.OriginLoopback}}
}

// originForAddr classifies a bind address as loopback or public. A loopback IP (or "localhost")
// binds the SSH-tunnel-only zone; anything else (incl. a wildcard bind) is reachable off-host and is
// treated as public. This is the tag only — the trust policy built on it is W7.
func originForAddr(addr string) adminhttp.ListenerOrigin {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return adminhttp.OriginLoopback
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return adminhttp.OriginLoopback
	}
	return adminhttp.OriginPublic
}

func whoami(w http.ResponseWriter, r *http.Request) {
	pr, _ := adminhttp.PrincipalFrom(r.Context())
	adminhttp.WriteOK(w, r, whoamiFields(pr))
}

// whoamiFields is the GET /api/whoami payload (sans the envelope's success), built from the normalised
// Principal so it answers identically for every carrier (design §4.3): the SPA's human login is a
// cookie session after W8, and it must report the session's real identity + admin flag, not the empty
// operator that only the legacy bearer carrier populated. Shape is {kind, is_admin, scopes, label}
// (design §4.3). The field is **is_admin** (snake_case) — the SPA read-only badge (D19.6) derives off
// it; a rename breaks T9, not silently the UI. NOT ctxd's `admin` field name.
func whoamiFields(pr adminhttp.Principal) map[string]any {
	return map[string]any{
		"kind":     pr.Kind,
		"is_admin": pr.IsAdmin,
		"label":    pr.Label,
		"scopes":   pr.Scopes,
	}
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
