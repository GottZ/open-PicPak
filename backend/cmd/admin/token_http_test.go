package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/adminsession"
	"github.com/open-picpak/backend/internal/adminuser"
	"github.com/open-picpak/backend/internal/apitoken"
)

func testTokenHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerTokenRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// sessionCookieFor opens a real admin_sessions row for userID (production write path) and returns the
// ppk_sid cookie carrying its plaintext secret.
func sessionCookieFor(t *testing.T, pool *pgxpool.Pool, userID int64) *http.Cookie {
	t.Helper()
	secret, _, err := adminsession.Create(context.Background(), pool, userID, time.Hour)
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: secret}
}

// doCookie drives a request authenticated by a session cookie, optionally carrying the CSRF header a
// mutating cookie request needs (design §4.1).
func doCookie(h http.Handler, method, path string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf {
		r.Header.Set("X-Requested-With", "picpak")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestTokenGating_DB — the W6 negative probes (1)+(2): every /api/tokens route is IsAdmin-gated. A
// mutation/read is 401 without a credential, 403 for a non-admin api_token bearer (even one carrying
// image:write) AND for a non-admin session, and past-the-gate for an admin. Routes come from
// registerTokenRoutes — the SAME wiring main.go mounts. Red (route without RequireAdmin): the api_token /
// non-admin session reaches the handler (2xx/4xx-non-403) instead of a 403.
func TestTokenGating_DB(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	seedOperator(t, pool, "admin-tok", true)
	ctx := context.Background()

	// A non-admin api_token WITH image:write — the strongest non-admin machine credential.
	ppk, _, err := apitoken.Create(ctx, pool, "ci", []string{"image:write"}, nil, nil)
	if err != nil {
		t.Fatalf("mint ppk: %v", err)
	}
	// A non-admin human session.
	u, err := adminuser.Create(ctx, pool, "nonadmin", "pw", false)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cookie := sessionCookieFor(t, pool, u.ID)

	h := testTokenHandler(pool)

	routes := []struct {
		method, path string
		mutating     bool
	}{
		{"POST", "/api/tokens", true},
		{"GET", "/api/tokens", false},
		{"DELETE", "/api/tokens/1", true},
	}
	for _, rt := range routes {
		if w := do(h, rt.method, rt.path, "", nil, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no credential = %d, want 401", rt.method, rt.path, w.Code)
		}
		if w := do(h, rt.method, rt.path, ppk, nil, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s %s api_token(image:write) = %d, want 403", rt.method, rt.path, w.Code)
		}
		// Non-admin session; a mutation carries the CSRF header so it reaches RequireAdmin (a miss would
		// be a 403 csrf_required and mask the is_admin gate we are proving).
		if w := doCookie(h, rt.method, rt.path, cookie, rt.mutating); w.Code != http.StatusForbidden {
			t.Errorf("%s %s non-admin session = %d, want 403", rt.method, rt.path, w.Code)
		}
		if w := do(h, rt.method, rt.path, "admin-tok", nil, ""); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("%s %s admin = %d, want past-the-gate", rt.method, rt.path, w.Code)
		}
	}
}

// TestTokenMint_OnceShown_NoLeak — the W6 probe (3): a mint returns the ppk_ plaintext EXACTLY ONCE, and
// no later read (GET /api/tokens) ever surfaces the token, its secret, or the stored hash. The contrast
// is the proof: a naive query that selects secret_hash gets 32 real bytes (what a leaky listing WOULD
// expose — red), while the API response omits every secret field (green). A token.mint audit row with
// target=token_id is written.
func TestTokenMint_OnceShown_NoLeak(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	seedOperator(t, pool, "admin-tok", true)
	h := testTokenHandler(pool)
	ctx := context.Background()

	w := do(h, "POST", "/api/tokens", "admin-tok",
		strings.NewReader(`{"label":"deploy","scopes":["image:read","image:write"]}`), "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("mint = %d (%s)", w.Code, w.Body.String())
	}
	body := jsonBody(t, w)
	plaintext, _ := body["token"].(string)
	if !strings.HasPrefix(plaintext, apitoken.TokenPrefix) {
		t.Fatalf("mint did not return a ppk_ token once: %q", plaintext)
	}
	tokenID, _ := body["token_id"].(string)
	if tokenID == "" {
		t.Fatal("mint response carried no token_id")
	}

	// Red side: the hash EXISTS at rest — a listing that selected it would leak key material.
	var hash []byte
	if err := pool.QueryRow(ctx, `SELECT secret_hash FROM api_tokens WHERE token_id=$1`, tokenID).Scan(&hash); err != nil {
		t.Fatalf("select secret_hash: %v", err)
	}
	if len(hash) != 32 {
		t.Fatalf("secret_hash at rest = %d bytes, want 32", len(hash))
	}

	// Green side: GET /api/tokens surfaces the token WITHOUT any secret material.
	lw := do(h, "GET", "/api/tokens", "admin-tok", nil, "")
	if lw.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", lw.Code, lw.Body.String())
	}
	raw := lw.Body.String()
	for _, banned := range []string{plaintext, "secret", "hash", strings.TrimPrefix(plaintext, apitoken.TokenPrefix)} {
		if strings.Contains(raw, banned) {
			t.Fatalf("listing leaked %q: %s", banned, raw)
		}
	}
	// The listing still names the token by its public handle + status.
	if !strings.Contains(raw, tokenID) || !strings.Contains(raw, "active") {
		t.Fatalf("listing missing token_id/status: %s", raw)
	}

	if n := sessionAuditCount(t, pool, "token.mint", tokenID); n != 1 {
		t.Fatalf("token.mint audit rows (target=%s) = %d, want 1", tokenID, n)
	}
}

// TestTokenRevoke_NextRequest401 — the W6 probes (4)+(5): DELETE soft-revokes a token and writes a
// synchronous token.revoke audit row (target=token_id); the revoked token authenticates as absent from
// its very next request. The 403->401 transition is the proof: BEFORE revoke the token authenticates (403
// non-admin on an admin route), AFTER revoke it is unauthenticated (401). Red (revoke not effective /
// disabled_at filter absent): the second request is still a 403.
func TestTokenRevoke_NextRequest401(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	seedOperator(t, pool, "admin-tok", true)
	h := testTokenHandler(pool)
	ctx := context.Background()

	ppk, tok, err := apitoken.Create(ctx, pool, "revoke-me", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("mint ppk: %v", err)
	}
	// Before revoke: the token authenticates (403 = authenticated, not admin) on an admin route.
	if w := do(h, "GET", "/api/tokens", ppk, nil, ""); w.Code != http.StatusForbidden {
		t.Fatalf("pre-revoke ppk request = %d, want 403 (authenticated non-admin)", w.Code)
	}

	dw := do(h, "DELETE", "/api/tokens/"+strconv.FormatInt(tok.ID, 10), "admin-tok", nil, "")
	if dw.Code != http.StatusOK {
		t.Fatalf("revoke = %d (%s)", dw.Code, dw.Body.String())
	}
	if n := sessionAuditCount(t, pool, "token.revoke", tok.TokenID); n != 1 {
		t.Fatalf("token.revoke audit rows (target=%s) = %d, want 1", tok.TokenID, n)
	}

	// After revoke: the same token is unauthenticated (401), effective on its next request.
	if w := do(h, "GET", "/api/tokens", ppk, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke ppk request = %d, want 401 (revoked -> unauthenticated)", w.Code)
	}

	// An unknown id is a uniform 404 (no enumeration signal).
	if w := do(h, "DELETE", "/api/tokens/999999", "admin-tok", nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown id = %d, want 404", w.Code)
	}
}

// TestCreateApiToken_NonTTY_Refusal — the W6 probe (6): the CLI mint refuses on a non-TTY stdout (the
// test process' stdout is not a terminal) BEFORE it opens the pool or mints, so no orphan token is left
// whose plaintext was captured to a log. Exit code 3, mirroring create-operator's S11 guard. Needs no DB.
func TestCreateApiToken_NonTTY_Refusal(t *testing.T) {
	if code := createApiToken(context.Background(), "boot", []string{"image:read"}, 0); code != 3 {
		t.Fatalf("createApiToken on non-TTY stdout = %d, want 3 (refusal)", code)
	}
}
