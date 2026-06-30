package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// DB property tests for the A22 telemetry routes (skipped unless TEST_DATABASE_URL is set). The shared
// harness (dbPool/mustExec/seedOperator) lives in ota_http_test.go.

// telemetryTestHandler mounts the telemetry routes PLUS Doc 17's device list/get, so the 22→17 seam
// (T5) can be probed across both surfaces in one mux.
func telemetryTestHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	dh := deviceHandlers{pool: pool}
	mux.Handle("GET /api/devices", adminhttp.Auth(pool)(http.HandlerFunc(dh.list)))
	mux.Handle("GET /api/devices/{serial}", adminhttp.Auth(pool)(http.HandlerFunc(dh.get)))
	registerTelemetryRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

func getJSON(h http.Handler, path, token string) (int, string) {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

type fleetResp struct {
	Success bool `json:"success"`
	Fleet   []struct {
		Serial          string    `json:"serial"`
		RunningVer      string    `json:"running_ver"`
		Health          string    `json:"health"`
		HasData         bool      `json:"has_data"`
		ChannelMismatch bool      `json:"channel_mismatch"`
		Time            time.Time `json:"time"`
	} `json:"fleet"`
	ServerTime time.Time `json:"server_time"`
}

func mustFleet(t *testing.T, body string) fleetResp {
	t.Helper()
	var fr fleetResp
	if err := json.Unmarshal([]byte(body), &fr); err != nil {
		t.Fatalf("unmarshal fleet: %v\n%s", err, body)
	}
	return fr
}

// T5 — the 22→17 seam holds: Doc 17's GET /api/devices carries NO running_ver; Doc 22's GET /api/fleet
// DOES. Red: re-adding running_ver to Doc 17's route inverts the seam (the foundation pulls a telemetry
// read) and regresses the topology.
func TestTelemetry_Seam22to17_T5(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "tok", false)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('DEV1','stable')`)
	mustExec(t, pool, `INSERT INTO telemetry (time, serial, running_ver, batt_pct) VALUES (now(),'DEV1','fw-7',80)`)
	h := telemetryTestHandler(pool)

	if _, dev := getJSON(h, "/api/devices", "tok"); strings.Contains(dev, "running_ver") {
		t.Errorf("Doc 17 /api/devices leaked running_ver — 22→17 seam inverted:\n%s", dev)
	}
	code, fleet := getJSON(h, "/api/fleet", "tok")
	if code != http.StatusOK {
		t.Fatalf("/api/fleet = %d, want 200", code)
	}
	fr := mustFleet(t, fleet)
	if len(fr.Fleet) != 1 || fr.Fleet[0].RunningVer != "fw-7" {
		t.Fatalf("/api/fleet must carry running_ver=fw-7; got %+v", fr.Fleet)
	}
}

// T8 — the empty/near-empty hypertable is the current normal, not a fault. A registered device with no
// telemetry → /api/fleet is 200, the row is NO_DATA, no 500/panic. Red: a scan assuming a telemetry row
// NULL-derefs.
func TestTelemetry_EmptyTableNoPanic_T8(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "tok", false)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('DEVQUIET','stable')`)
	h := telemetryTestHandler(pool)

	code, body := getJSON(h, "/api/fleet", "tok")
	if code != http.StatusOK {
		t.Fatalf("/api/fleet over an empty hypertable = %d, want 200", code)
	}
	fr := mustFleet(t, body)
	if len(fr.Fleet) != 1 || fr.Fleet[0].HasData || fr.Fleet[0].Health != "NO_DATA" {
		t.Fatalf("a silent device must render NO_DATA, not an all-n/a fault; got %+v", fr.Fleet)
	}
}

// T10 — the freshness clocks are distinct: server_time (now) is sent separately from each row's telemetry
// time, so a stale table never reads "live". Red: collapsing them makes an hours-old row look current.
func TestTelemetry_FreshnessClocksDistinct_T10(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "tok", false)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('DEVOLD','stable')`)
	mustExec(t, pool, `INSERT INTO telemetry (time, serial, batt_pct) VALUES (now() - interval '5 hours','DEVOLD',60)`)
	h := telemetryTestHandler(pool)

	_, body := getJSON(h, "/api/fleet", "tok")
	fr := mustFleet(t, body)
	if fr.ServerTime.IsZero() {
		t.Fatal("server_time must be present (the poll clock)")
	}
	if len(fr.Fleet) != 1 {
		t.Fatalf("want 1 row; got %d", len(fr.Fleet))
	}
	if gap := fr.ServerTime.Sub(fr.Fleet[0].Time); gap < time.Hour {
		t.Fatalf("server_time and the row's telemetry time must be distinct clocks; gap=%s", gap)
	}
}

// T13 — the Grafana + webhook base URLs are config-gated via env, never hardcoded (air-gap). Unset →
// /api/config returns empty strings (the SPA hides the links); set → it returns them. Red: a URL literal
// shipped in the SPA is an air-gap breach plus a dead link on every other deployment.
func TestTelemetry_ConfigGated_T13(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "tok", false)

	type cfgResp struct {
		GrafanaBaseURL string `json:"grafana_base_url"`
		WebhookBaseURL string `json:"webhook_base_url"`
	}
	parse := func(body string) cfgResp {
		var c cfgResp
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("unmarshal config: %v\n%s", err, body)
		}
		return c
	}

	// unset → empty (handler reads env at register time, so build the handler under the empty env).
	t.Setenv("ADMIN_GRAFANA_BASE_URL", "")
	t.Setenv("ADMIN_WEBHOOK_BASE_URL", "")
	_, body := getJSON(telemetryTestHandler(pool), "/api/config", "tok")
	if c := parse(body); c.GrafanaBaseURL != "" || c.WebhookBaseURL != "" {
		t.Fatalf("unset env must yield empty config; got %+v", c)
	}

	// set → returned verbatim (synthetic example URLs — never a real private host in the test either).
	t.Setenv("ADMIN_GRAFANA_BASE_URL", "https://grafana.example/d/picpak")
	t.Setenv("ADMIN_WEBHOOK_BASE_URL", "https://hooks.example")
	_, body = getJSON(telemetryTestHandler(pool), "/api/config", "tok")
	if c := parse(body); c.GrafanaBaseURL != "https://grafana.example/d/picpak" || c.WebhookBaseURL != "https://hooks.example" {
		t.Fatalf("set env must be returned; got %+v", c)
	}
}

// Read-only gate (the A22 mirror of the log viewer Q5): all three telemetry routes are auth-gated, never
// RequireAdmin — a read-only operator must reach the dashboard; unauthenticated is 401.
func TestTelemetry_ReadOnlyGate(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "ro", false) // NON-admin
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('DEVR','stable')`)
	mustExec(t, pool, `INSERT INTO telemetry (time, serial, batt_pct) VALUES (now(),'DEVR',70)`)
	h := telemetryTestHandler(pool)

	for _, path := range []string{"/api/fleet", "/api/devices/DEVR/telemetry", "/api/config"} {
		if c, _ := getJSON(h, path, "ro"); c != http.StatusOK {
			t.Errorf("GET %s as read-only operator = %d, want 200", path, c)
		}
		if c, _ := getJSON(h, path, ""); c != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated = %d, want 401", path, c)
		}
	}

	// an unregistered serial → 404 (FleetForSerials empty), not a 200 with an empty card.
	if c, _ := getJSON(h, "/api/devices/NOPE/telemetry", "ro"); c != http.StatusNotFound {
		t.Errorf("unregistered serial telemetry = %d, want 404", c)
	}
}
