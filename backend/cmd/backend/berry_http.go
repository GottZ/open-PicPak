package main

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/berry"
	"github.com/open-picpak/backend/internal/commandstore"
)

// Berry editor read surface (A23 / Design 23 §4.2, W1). The capability manifest is the editor's catalog
// — the C2 safe-subset surface it autocompletes and lints against. It is declared DATA (go:embed'd
// manifest.json in internal/berry), parity-tested bidirectionally against the firmware be_regfunc sites
// (D23.2/T6), and served here at GET /api/berry/capabilities.
//
// Auth-gated, never RequireAdmin (Q5/D19.6): a read-only operator can author + lint a draft (useful
// offline); only enqueue (W3) is admin-gated. W1 ships NO write path — author-only, no RCE.

// berryHandlers serves the recent-commands rehydration read; the window/cap are env (Policy=Data).
type berryHandlers struct {
	pool         *pgxpool.Pool
	recentWindow time.Duration
	recentMax    int
}

// registerBerryRoutes mounts the capability endpoint + the recent-commands rehydration read. Both are
// literal patterns, so stdlib ServeMux precedence keeps them distinct from every other /api/... route
// (registration order irrelevant). Both auth-gated, never RequireAdmin — read-only operators reach them.
func registerBerryRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := berryHandlers{
		pool:         pool,
		recentWindow: envDurOr("ADMIN_BERRY_RECENT_WINDOW", 24*time.Hour),
		recentMax:    envIntOr("ADMIN_BERRY_RECENT_MAX", 100),
	}
	mux.Handle("GET /api/berry/capabilities", adminhttp.Auth(pool)(http.HandlerFunc(berryCapabilities)))
	mux.Handle("GET /api/berry/commands/recent", adminhttp.Auth(pool)(http.HandlerFunc(h.recent)))
}

// recent — GET /api/berry/commands/recent (auth): the rehydration source (D23.9). On mount/reload the
// editor seeds its feedback view from this (recent command_queue rows + per-command applied/target
// counts), then the c2cursor SSE carries live deltas — SSE alone is a forward diff with no enqueue
// history, so it cannot reconstruct the N-of-M view after a refresh. The script is NOT returned (air-gap,
// T11). Read-only → visible under read-only degradation (§4.6).
func (h berryHandlers) recent(w http.ResponseWriter, r *http.Request) {
	cmds, err := commandstore.RecentCommands(r.Context(), h.pool, h.recentWindow, h.recentMax)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "recent commands query failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"commands": cmds})
}

// berryCapabilities — GET /api/berry/capabilities (auth): the manifest catalog. The fields are spread
// at the top level by the envelope (K10): {success, capabilities:[…], builtins:[…]}. The manifest
// carries no example argument values (only param names/types) — air-gap-clean by construction (T10).
func berryCapabilities(w http.ResponseWriter, r *http.Request) {
	m := berry.Get()
	adminhttp.WriteOK(w, r, map[string]any{
		"capabilities": m.Capabilities,
		"builtins":     m.Builtins,
	})
}
