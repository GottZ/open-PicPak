package main

import (
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
