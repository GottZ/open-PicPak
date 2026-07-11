package main

import (
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// onboardDefaults serves the fully tokenized device frame-/C2-URLs for the /onboard prefill (A35.3b,
// DECISIONS §Nachträge "A35-W3b"). The SPA's A35.3 prefill only carries the "<INGEST_TOKEN>" PLACEHOLDER
// because the secret is server-side-only; this endpoint hands the REAL token to an ADMIN session so the
// operator does not have to paste the token segment by hand.
//
// Governance (DECISIONS §A35-W3b): the INGEST_TOKEN is now HTTP-readable to an admin session —
// deliberately. Onboarding enroll is already admin-gated, and an admin session can enqueue RCE commands;
// the ingest path token is strictly WEAKER than the capability the session already holds. So it is gated
// exactly like the fleet mutations: adminhttp.Auth + adminhttp.RequireAdmin (401 no bearer / 403 non-admin).
//
// Token source (coupling decision): cmd/backend reads INGEST_TOKEN from the process env DIRECTLY, the
// SAME os.Getenv the ingest Server reads (internal/ingestcore/server.go:62). It does NOT reach into
// ingestcore.Server (built later, at the root handler) nor add an exporter method — that would be a NEW
// coupling for one string both sides already read from the identical env var. Empty token ⇒ empty URLs
// (feature dark), so a deployment without INGEST_TOKEN answers 200 with blank URLs and the SPA keeps its
// origin-placeholder fallback.
func onboardDefaults(w http.ResponseWriter, r *http.Request) {
	token := os.Getenv("INGEST_TOKEN") // legacy path token; kept in env, never in code (air-gap)
	frameURL, c2URL := "", ""
	if token != "" {
		// The device endpoint is same-origin under A35's ONE backend (DECISIONS §A35). The SPA passes its
		// authoritative location.origin; a bare call falls back to the request host so a manual curl works.
		origin := strings.TrimRight(r.URL.Query().Get("origin"), "/")
		if origin == "" {
			scheme := "http"
			if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
				scheme = "https"
			}
			origin = scheme + "://" + r.Host
		}
		seg := origin + "/" + token // path scheme mirrors ingestcore/server.go Handle: /<token>/{frame,c2}
		frameURL = seg + "/frame"
		c2URL = seg + "/c2"
	}
	adminhttp.WriteOK(w, r, map[string]any{
		"frame_url":   frameURL,
		"c2_url":      c2URL,
		"c2_period_s": onboardDefaultC2PeriodSeconds, // design/26 §2.2 / D26.4 form default
	})
}

// onboardDefaultC2PeriodSeconds mirrors the SPA's DEFAULT_C2_PERIOD_SECONDS (lib/onboard/defaults.ts).
const onboardDefaultC2PeriodSeconds = 1800

// registerOnboardDefaultsRoutes mounts GET /api/onboard/defaults, admin-gated (Auth + RequireAdmin).
func registerOnboardDefaultsRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	mux.Handle("GET /api/onboard/defaults",
		adminhttp.Auth(pool)(adminhttp.RequireAdmin(http.HandlerFunc(onboardDefaults))))
}
