package main

import (
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/telemetry"
)

// Telemetry dashboard read surface (A22 / Design 22 §4.2). THREE routes, all auth-gated (any valid key,
// never RequireAdmin — a read-only operator must reach the dashboard, mirroring the log viewer Q5). The
// query mechanics live in internal/telemetry (read-only, D22.1); policy (thresholds, caps, base URLs) is
// ADMIN_TELEMETRY_*/ADMIN_*_BASE_URL env here, the owning process (mechanism=code / policy=data, D22.10).
// telemetryPolicy is the env-driven Policy=Data for the telemetry surface (verdict thresholds, caps,
// base URLs). It is loaded once and shared by BOTH the REST routes (registerTelemetryRoutes) and the SSE
// producer (newEventsHandler, events.go) so the dashboard and the live stream agree on one verdict
// authority (D22.4) and one set of thresholds.
type telemetryPolicy struct {
	cfg                 telemetry.VerdictCfg // the per-device verdict thresholds
	fleetMax            int                  // ADMIN_TELEMETRY_FLEET_MAX_DEVICES
	historyWindow       time.Duration        // ADMIN_TELEMETRY_HISTORY_WINDOW
	historyMaxRows      int                  // ADMIN_TELEMETRY_HISTORY_MAX_ROWS
	channelMismatchWarn bool                 // ADMIN_TELEMETRY_CHANNEL_MISMATCH_WARN — gates the flag
	grafanaBaseURL      string               // ADMIN_GRAFANA_BASE_URL — unset → SPA hides the deep-link
	webhookBaseURL      string               // ADMIN_WEBHOOK_BASE_URL — unset → editor hides the webhook URL
}

func loadTelemetryPolicy() telemetryPolicy {
	return telemetryPolicy{
		cfg: telemetry.VerdictCfg{
			StaleAfter:           envDurOr("ADMIN_TELEMETRY_STALE_AFTER", 3*time.Hour),
			BrownoutResetReasons: envCSV("ADMIN_TELEMETRY_BROWNOUT_RESET_REASONS", []string{"brownout"}),
			BrownoutOTARRCodes:   envIntCSV("ADMIN_TELEMETRY_BROWNOUT_OTA_RR_CODES", []int{9}),
			BadBootsWarn:         envIntOr("ADMIN_TELEMETRY_BAD_BOOTS_WARN", 2),
			LowBattPct:           envIntOr("ADMIN_TELEMETRY_LOW_BATT_PCT", 15),
			LowBattIncludesUSB:   envBool("ADMIN_TELEMETRY_LOW_BATT_INCLUDES_USB", false),
		},
		fleetMax:            envIntOr("ADMIN_TELEMETRY_FLEET_MAX_DEVICES", 1000),
		historyWindow:       envDurOr("ADMIN_TELEMETRY_HISTORY_WINDOW", 72*time.Hour),
		historyMaxRows:      envIntOr("ADMIN_TELEMETRY_HISTORY_MAX_ROWS", 500),
		channelMismatchWarn: envBool("ADMIN_TELEMETRY_CHANNEL_MISMATCH_WARN", true),
		grafanaBaseURL:      os.Getenv("ADMIN_GRAFANA_BASE_URL"),
		webhookBaseURL:      os.Getenv("ADMIN_WEBHOOK_BASE_URL"),
	}
}

// telemetryHandlers serves the REST routes; it embeds the shared policy (its fields are promoted, so the
// handlers read h.cfg/h.fleetMax/… directly).
type telemetryHandlers struct {
	repo *telemetry.Repo
	telemetryPolicy
}

// registerTelemetryRoutes mounts the 3 read routes on Doc 17's mux. GET /api/devices/{serial}/telemetry
// is MORE specific than Doc 17's GET /api/devices/{serial}, so stdlib longest-pattern precedence routes
// each correctly (no conflict, registration order irrelevant).
func registerTelemetryRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := telemetryHandlers{repo: telemetry.NewRepo(pool), telemetryPolicy: loadTelemetryPolicy()}
	mux.Handle("GET /api/fleet", adminhttp.Auth(pool)(http.HandlerFunc(h.fleet)))
	mux.Handle("GET /api/devices/{serial}/telemetry", adminhttp.Auth(pool)(http.HandlerFunc(h.deviceTelemetry)))
	mux.Handle("GET /api/config", adminhttp.Auth(pool)(http.HandlerFunc(h.config)))
}

// fleetView is one fleet row on the wire: the enriched FleetRow (embedded → its json fields are promoted,
// each Null[T] rendering value-or-null) plus the SERVER-COMPUTED verdict (one verdict authority, D22.4)
// and the channel-mismatch flag. running_ver lives HERE, never on Doc 17's GET /api/devices — the 22→17
// seam (T5).
type fleetView struct {
	telemetry.FleetRow
	Health          string   `json:"health"`
	Reasons         []string `json:"reasons"`
	ChannelMismatch bool     `json:"channel_mismatch"`
}

func (h telemetryHandlers) viewOf(row telemetry.FleetRow, now time.Time) fleetView {
	hh, reasons := telemetry.Verdict(row.Sample(), h.cfg, now)
	return fleetView{
		FleetRow:        row,
		Health:          hh.String(),
		Reasons:         reasons,
		ChannelMismatch: h.channelMismatchWarn && row.ChannelMismatch(),
	}
}

// fleet — GET /api/fleet (auth): the enriched fleet list (D22.2/D22.3). One row per REGISTERED device
// (a silent device survives as NO_DATA, T1); each carries running_ver/batt/health + assigned vs reported
// channel + both freshness clocks. server_time is sent SEPARATELY from each row's last_seen/c2_last_seen
// so the SPA shows all three clocks honestly and a stale table never reads "live" (T10).
func (h telemetryHandlers) fleet(w http.ResponseWriter, r *http.Request) {
	rows, err := h.repo.LatestFleet(r.Context(), h.fleetMax)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "fleet query failed")
		return
	}
	now := time.Now()
	views := make([]fleetView, 0, len(rows))
	for _, row := range rows {
		views = append(views, h.viewOf(row, now))
	}
	adminhttp.WriteOK(w, r, map[string]any{"fleet": views, "server_time": now})
}

// deviceTelemetry — GET /api/devices/{serial}/telemetry (auth): the per-device card data. latest is the
// full sentinel-aware row (null when the device has no telemetry — NO_DATA, never a panic, D22.9);
// history is the double-bounded sparkline window (D22.8). The verdict authority is the device's fleet row
// (c2-aware NO_DATA split, consistent with /api/fleet); rollback_brownout is the cross-row corroboration
// chip over the two newest history points (computed only over history, D22.4). An unregistered serial → 404.
func (h telemetryHandlers) deviceTelemetry(w http.ResponseWriter, r *http.Request) {
	serial := r.PathValue("serial")
	fleetRows, err := h.repo.FleetForSerials(r.Context(), []string{serial}, 1)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "fleet lookup failed")
		return
	}
	if len(fleetRows) == 0 {
		adminhttp.WriteErr(w, r, http.StatusNotFound, "not_found", "no such device")
		return
	}
	latest, err := h.repo.LatestDevice(r.Context(), serial)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "device telemetry query failed")
		return
	}
	hist, err := h.repo.DeviceHistory(r.Context(), serial, h.historyWindow, h.historyMaxRows)
	if err != nil {
		adminhttp.WriteErr(w, r, http.StatusInternalServerError, "internal", "device history query failed")
		return
	}
	now := time.Now()
	adminhttp.WriteOK(w, r, map[string]any{
		"fleet":             h.viewOf(fleetRows[0], now), // header: health/channel/clocks (verdict authority)
		"latest":            latest,                      // full diag_*+extra detail grid (null if NO_DATA)
		"history":           hist,                        // sparkline window
		"rollback_brownout": rollbackFires(hist, h.cfg),  // cross-row brownout chip
		"server_time":       now,
	})
}

// config — GET /api/config (auth): the non-secret SPA config (D22.13). Empty strings when the env is
// unset → the SPA hides the Grafana deep-link / the webhook URL (no hardcoded URL ships — air-gap, T13).
func (h telemetryHandlers) config(w http.ResponseWriter, r *http.Request) {
	adminhttp.WriteOK(w, r, map[string]any{
		"grafana_base_url": h.grafanaBaseURL,
		"webhook_base_url": h.webhookBaseURL,
	})
}

// rollbackFires reports the cross-row brownout corroboration over the two NEWEST history points (history
// is newest-first). A sentinel/NULL uptime is skipped inside RollbackChip, so a "not measured" value
// never reads as a drop → false brownout (D22.5).
func rollbackFires(hist []telemetry.HistoryPoint, cfg telemetry.VerdictCfg) bool {
	if len(hist) < 2 {
		return false
	}
	return telemetry.RollbackChip(hist[0].Sample(), hist[1].Sample(), cfg)
}

// ---- env helpers (Policy=Data; env-only, no config file — design 19/22 §5) ----

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// envCSV reads a comma-separated string list; unset → def (csvParam trims + drops empties, logs_http.go).
func envCSV(k string, def []string) []string {
	if v := os.Getenv(k); v != "" {
		if out := csvParam(v); len(out) > 0 {
			return out
		}
	}
	return def
}

// envIntCSV reads a comma-separated int list; unset/all-unparseable → def.
func envIntCSV(k string, def []int) []int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var out []int
	for _, p := range csvParam(v) {
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
