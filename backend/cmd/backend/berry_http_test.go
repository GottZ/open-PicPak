package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
)

// DB property test for the A23 W1 capabilities endpoint (skipped unless TEST_DATABASE_URL is set —
// Auth resolves the bearer against operator_keys). The shared harness (dbPool/seedOperator) lives in
// ota_http_test.go; getJSON lives in telemetry_http_test.go (same package).

func berryTestHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerBerryRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

type capabilitiesResp struct {
	Success      bool `json:"success"`
	Capabilities []struct {
		Name  string `json:"name"`
		Class string `json:"class"`
		Risk  string `json:"risk"`
	} `json:"capabilities"`
	Builtins []string `json:"builtins"`
}

// reMAC catches a colon- or dash-separated 6-octet MAC; reURL catches any scheme://host literal.
// Either in the served manifest is an air-gap breach.
var (
	reMAC = regexp.MustCompile(`(?i)\b[0-9a-f]{2}([:-][0-9a-f]{2}){5}\b`)
	reURL = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://`)
)

// T10 — the capabilities endpoint is auth-gated AND air-gap-clean. Unauthenticated → 401; a valid
// (even read-only) key → 200 with the catalog; the body carries no real serial/URL/SSID. Red:
// dropping Auth makes it a 200 hole; a hardcoded example URL/MAC in the manifest is an air-gap breach.
func TestBerry_CapabilitiesAuthAndAirgap_T10(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "ro", false) // NON-admin reaches the read-only catalog
	h := berryTestHandler(pool)

	// unauthenticated → 401
	if c, _ := getJSON(h, "/api/berry/capabilities", ""); c != http.StatusUnauthorized {
		t.Errorf("GET /api/berry/capabilities unauthenticated = %d, want 401", c)
	}

	// read-only key → 200 + catalog
	code, body := getJSON(h, "/api/berry/capabilities", "ro")
	if code != http.StatusOK {
		t.Fatalf("GET /api/berry/capabilities as read-only = %d, want 200", code)
	}

	var cr capabilitiesResp
	if err := json.Unmarshal([]byte(body), &cr); err != nil {
		t.Fatalf("unmarshal capabilities: %v\n%s", err, body)
	}
	if !cr.Success || len(cr.Capabilities) == 0 || len(cr.Builtins) == 0 {
		t.Fatalf("expected a non-empty catalog + builtins; got success=%v caps=%d builtins=%d",
			cr.Success, len(cr.Capabilities), len(cr.Builtins))
	}

	// the catalog must actually distinguish the risk/forbidden surfaces the linter depends on.
	var sawSevering, sawForbidden bool
	for _, c := range cr.Capabilities {
		if c.Risk == "severing" {
			sawSevering = true
		}
		if c.Class == "forbidden" {
			sawForbidden = true
		}
	}
	if !sawSevering {
		t.Error("catalog carries no risk:severing capability — the severing-warning lint (D23.8) has no data")
	}
	if !sawForbidden {
		t.Error("catalog carries no forbidden-class capability — the forbidden-surface lint (D23.3) has no data")
	}

	// air-gap: no real URL / MAC literal in the served manifest (it carries only param names+types).
	if loc := reURL.FindString(body); loc != "" {
		t.Errorf("air-gap breach: served capabilities contain a URL literal %q", loc)
	}
	if loc := reMAC.FindString(body); loc != "" {
		t.Errorf("air-gap breach: served capabilities contain a MAC literal %q", loc)
	}
	if strings.Contains(strings.ToLower(body), "gottz") {
		t.Errorf("air-gap breach: served capabilities reference a private host")
	}
}

type recentResp struct {
	Success  bool `json:"success"`
	Commands []struct {
		Seq          int64  `json:"seq"`
		Serial       string `json:"serial"`
		Note         *string `json:"note"`
		TargetCount  int    `json:"target_count"`
		AppliedCount int    `json:"applied_count"`
	} `json:"commands"`
}

// T11 — the recent-enqueue rehydration source (D23.9). GET /api/berry/commands/recent returns recent
// command_queue rows joined to device_c2_cursor with a correct per-device applied/pending count, incl.
// the N-of-M breakdown for a '*' row; auth-gated; air-gap-clean (the SCRIPT — which can carry a URL/SSID
// — is NOT in the body). Red: without it, a reloaded SPA shows a blank N-of-M view until the next cursor.
func TestBerry_RecentRehydration_T11(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "ro", false) // read-only reaches the rehydration read (D23.9 / §4.6)
	ctx := context.Background()

	for _, s := range []string{"DEV1", "DEV2", "DEV3"} {
		mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ($1,'stable')`, s)
	}

	// Insert three commands; seq is GENERATED so capture each. A script with a URL — to prove it is
	// EXCLUDED from the rehydration body (air-gap T11).
	enqueue := func(serial, script, note string) int64 {
		var seq int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO command_queue (serial, script, note) VALUES ($1,$2,$3) RETURNING seq`,
			serial, script, note).Scan(&seq); err != nil {
			t.Fatalf("enqueue %s: %v", serial, err)
		}
		return seq
	}
	seq1 := enqueue("DEV1", `set_url("https://secret.example/ingest")`, "to dev1")
	seq2 := enqueue("DEV2", "reboot()", "to dev2")
	seq3 := enqueue("*", "refresh()", "broadcast")

	// Cursors: DEV1 & DEV3 reached seq3; DEV2 only seq1. So cmd1(DEV1)=applied, cmd2(DEV2)=pending,
	// cmd3('*')=2 of 3 applied (DEV1+DEV3 >= seq3, DEV2 < seq3).
	mustExec(t, pool, `INSERT INTO device_c2_cursor (serial, applied_seq, last_poll_at) VALUES ('DEV1',$1,now())`, seq3)
	mustExec(t, pool, `INSERT INTO device_c2_cursor (serial, applied_seq, last_poll_at) VALUES ('DEV2',$1,now())`, seq1)
	mustExec(t, pool, `INSERT INTO device_c2_cursor (serial, applied_seq, last_poll_at) VALUES ('DEV3',$1,now())`, seq3)

	h := berryTestHandler(pool)

	// auth-gated
	if c, _ := getJSON(h, "/api/berry/commands/recent", ""); c != http.StatusUnauthorized {
		t.Errorf("recent unauthenticated = %d, want 401", c)
	}

	code, body := getJSON(h, "/api/berry/commands/recent", "ro")
	if code != http.StatusOK {
		t.Fatalf("recent as read-only = %d, want 200", code)
	}

	// air-gap: the script (and its URL) must NOT appear in the rehydration body.
	if strings.Contains(body, "secret.example") || reURL.MatchString(body) {
		t.Errorf("air-gap breach: recent body leaked the script / a URL:\n%s", body)
	}

	var rr recentResp
	if err := json.Unmarshal([]byte(body), &rr); err != nil {
		t.Fatalf("unmarshal recent: %v\n%s", err, body)
	}
	if len(rr.Commands) != 3 {
		t.Fatalf("want 3 recent commands; got %d", len(rr.Commands))
	}
	// newest-first ordering: seq3, seq2, seq1
	by := map[int64]struct{ target, applied int }{}
	for _, c := range rr.Commands {
		by[c.Seq] = struct{ target, applied int }{c.TargetCount, c.AppliedCount}
	}
	if g := by[seq1]; g.target != 1 || g.applied != 1 {
		t.Errorf("cmd to DEV1 (cursor past it) must be 1/1 applied; got %+v", g)
	}
	if g := by[seq2]; g.target != 1 || g.applied != 0 {
		t.Errorf("cmd to DEV2 (cursor behind it) must be 0/1 (pending); got %+v", g)
	}
	if g := by[seq3]; g.target != 3 || g.applied != 2 {
		t.Errorf("'*' command must be 2-of-3 applied (DEV1+DEV3 reached, DEV2 behind); got %+v", g)
	}
}
