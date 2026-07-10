package adminhttp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/adminsession"
	"github.com/open-picpak/backend/internal/adminuser"
	"github.com/open-picpak/backend/internal/apitoken"
)

// mutReq drives one POST (mutating) request with the given credentials through the handler.
func mutReq(t *testing.T, h http.Handler, bearer, basicUser, basicPass, cookie, xrw string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/probe", nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if basicUser != "" {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(basicUser+":"+basicPass)))
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if xrw != "" {
		r.Header.Set(csrfHeader, xrw)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestCSRF_CookieCarrierOnly is the §4.1 CSRF negative probe: a MUTATING cookie-authed request without
// X-Requested-With: picpak is a 403 csrf_required (red: 200 — the cookie would be a one-layer ambient
// credential from W5 on); with the header it passes; a GET needs no header; and the non-ambient
// carriers (ppk_ bearer, Basic) mutate WITHOUT the header — they are CSRF-immune by construction.
func TestCSRF_CookieCarrierOnly(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(60*time.Second, 1024)
	ctx := context.Background()
	var seen Principal
	h := probeChain(pool, nil, &seen)

	u, err := adminuser.Create(ctx, pool, "kim", "pw-kim", true)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	secret, _, err := adminsession.Create(ctx, pool, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Mutating cookie request WITHOUT the header → 403 csrf_required (not the uniform 401).
	rec := mutReq(t, h, "", "", "", secret, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie mutation without header: want 403, got %d (%s)", rec.Code, rec.Body)
	}
	if got := decode(t, rec.Body.Bytes())["code"]; got != "csrf_required" {
		t.Fatalf("code = %v, want csrf_required", got)
	}
	// A wrong header value is as absent.
	if rec := mutReq(t, h, "", "", "", secret, "XMLHttpRequest"); rec.Code != http.StatusForbidden {
		t.Fatalf("cookie mutation with wrong header value: want 403, got %d", rec.Code)
	}

	// WITH the header → through the gate.
	if rec := mutReq(t, h, "", "", "", secret, csrfHeaderValue); rec.Code != http.StatusOK {
		t.Fatalf("cookie mutation with header: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	// A cookie GET needs no header (read-only, no state change).
	if rec := req(t, h, "", secret); rec.Code != http.StatusOK {
		t.Fatalf("cookie GET without header: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	// Bearer (ppk_) mutates WITHOUT the header — not an ambient credential.
	tok, _, err := apitoken.Create(ctx, pool, "writer", []string{"image:write"}, nil, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if rec := mutReq(t, h, tok, "", "", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("bearer mutation without header: want 200, got %d (%s)", rec.Code, rec.Body)
	}

	// Basic mutates WITHOUT the header — not ambient either (no WWW-Authenticate is ever issued, so a
	// browser never caches Basic creds for this origin).
	if rec := mutReq(t, h, "", "kim", "pw-kim", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("basic mutation without header: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if seen.Kind != KindBasic {
		t.Fatalf("last principal kind = %v, want basic", seen.Kind)
	}
}
