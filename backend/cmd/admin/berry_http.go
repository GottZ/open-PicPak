package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/berry"
)

// Berry editor read surface (A23 / Design 23 §4.2, W1). The capability manifest is the editor's catalog
// — the C2 safe-subset surface it autocompletes and lints against. It is declared DATA (go:embed'd
// manifest.json in internal/berry), parity-tested bidirectionally against the firmware be_regfunc sites
// (D23.2/T6), and served here at GET /api/berry/capabilities.
//
// Auth-gated, never RequireAdmin (Q5/D19.6): a read-only operator can author + lint a draft (useful
// offline); only enqueue (W3) is admin-gated. W1 ships NO write path — author-only, no RCE.

// registerBerryRoutes mounts the capability endpoint. /api/berry/capabilities is a literal pattern, so
// stdlib ServeMux precedence keeps it distinct from every other /api/... route (registration order
// irrelevant).
func registerBerryRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	mux.Handle("GET /api/berry/capabilities", adminhttp.Auth(pool)(http.HandlerFunc(berryCapabilities)))
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
