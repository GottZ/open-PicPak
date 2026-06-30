package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/logquery"
)

// Log viewer read surface (A21 / Design 21 part b). ALL three routes are auth-gated (any valid key,
// D21.7) — never RequireAdmin: logs are read-only with no mutation affordance, so a read-only operator
// must reach them (Q5). Policy (separator, page cap, windows) is env here (the owning process); the query
// mechanics live in internal/logquery, which never imports the ingest reassembly side (D21.1).
type logHandlers struct {
	pool          *pgxpool.Pool
	sep           string        // LOG_LINE_SEPARATOR (D21.10)
	pageMax       int           // LOG_PAGE_MAX — hard cap on a page
	serialsWindow time.Duration // LOG_SERIALS_WINDOW — the filter-picker horizon
}

// registerLogRoutes mounts the 3 read routes. /api/logs/serials is a literal that wins over the
// /api/logs/{serial}/reconstruct wildcard (ServeMux precedence), so the picker source never shadows.
func registerLogRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := logHandlers{
		pool:          pool,
		sep:           env("LOG_LINE_SEPARATOR", "|"),
		pageMax:       envIntOr("LOG_PAGE_MAX", 500),
		serialsWindow: envDurOr("LOG_SERIALS_WINDOW", 90*24*time.Hour),
	}
	mux.Handle("GET /api/logs", adminhttp.Auth(pool)(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/logs/serials", adminhttp.Auth(pool)(http.HandlerFunc(h.serials)))
	mux.Handle("GET /api/logs/{serial}/reconstruct", adminhttp.Auth(pool)(http.HandlerFunc(h.reconstruct)))
}

// list — GET /api/logs: a keyset page of reassembled log lines. Filters: serials (csv, empty=all),
// sources (csv, default telemetry+ota-snapshot), boot (epoch), before (opaque cursor), limit (≤pageMax).
func (h logHandlers) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := logquery.Params{
		Serials: csvParam(q.Get("serials")),
		Sources: csvParam(q.Get("sources")),
		Before:  q.Get("before"),
		Sep:     h.sep,
		Limit:   h.pageMax,
	}
	if s := q.Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < h.pageMax {
			p.Limit = n // a request over the cap is silently clamped to pageMax (no magic 4xx)
		}
	}
	if s := q.Get("boot"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_boot", "boot must be an integer epoch")
			return
		}
		p.Boot = &n
	}
	if p.Before != "" { // validate the cursor at the edge → 422, not a 500 from deep in the query
		if _, err := logquery.DecodeCursor(p.Before); err != nil {
			adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_cursor", "malformed pagination cursor")
			return
		}
	}
	page, err := logquery.Query(r.Context(), h.pool, p)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "log query failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{
		"lines": page.Lines, "next_cursor": page.NextCursor, "reached_edge": page.ReachedEdge,
	})
}

// serials — GET /api/logs/serials: distinct serials with logs in the recent window (the filter picker).
func (h logHandlers) serials(w http.ResponseWriter, r *http.Request) {
	ss, err := logquery.DistinctSerials(r.Context(), h.pool, h.serialsWindow)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "serials query failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{"serials": ss})
}

// reconstruct — GET /api/logs/{serial}/reconstruct?epoch=<e>: the gapless plaintext rebuild for one
// (serial, epoch) via span-merge (§4.4). epoch is required (a stream segment is one boot epoch).
func (h logHandlers) reconstruct(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	if serial == "" {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_serial", "serial required")
		return
	}
	epoch, err := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusUnprocessableEntity, "invalid_epoch", "epoch query param must be an integer")
		return
	}
	rec, err := logquery.Reconstruct(r.Context(), h.pool, serial, epoch)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "reconstruct failed")
		return
	}
	adminhttp.WriteOK(w, r, map[string]any{
		"serial": rec.Serial, "epoch": rec.Epoch, "text": rec.Text, "gaps": rec.Gaps,
	})
}

// startFragmentPrune runs the minimal day-one fragment prune (§4.7) on a ticker until ctx is cancelled —
// the fragment table has no retention policy of its own, so reassembly would grow it unbounded.
func startFragmentPrune(ctx context.Context, pool *pgxpool.Pool) {
	ttl := envDurOr("LOG_FRAGMENT_TTL", 90*24*time.Hour)
	interval := envDurOr("LOG_FRAGMENT_PRUNE_INTERVAL", 6*time.Hour)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				switch n, err := logquery.PruneFragments(ctx, pool, ttl); {
				case err != nil:
					log.Printf("log fragment prune: %v", err)
				case n > 0:
					log.Printf("log fragment prune removed %d rows", n)
				}
			}
		}
	}()
}

// csvParam splits a comma-separated query value into a trimmed, empty-dropped slice (nil = absent = all).
func csvParam(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDurOr(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
