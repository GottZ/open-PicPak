package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/adminuser"
)

func testAuthHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	registerAuthRoutes(mux, pool)
	return adminhttp.WithRequestID(mux)
}

// resetAuthTables clears the A28 auth tables the shared dbPool does not (admin_users CASCADE also
// clears admin_sessions; admin_audit is append-only so it is truncated explicitly).
func resetAuthTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		`TRUNCATE admin_users RESTART IDENTITY CASCADE`,
		`TRUNCATE admin_audit RESTART IDENTITY`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
}

func sessionAuditCount(t *testing.T, pool *pgxpool.Pool, action, target string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit WHERE action=$1 AND target=$2`, action, target).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func postSession(h http.Handler, jsonBody string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/session", strings.NewReader(jsonBody))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func sessionCookieFrom(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

// TestSession_WrongPassword_Uniform401 is the W3 negative probe (1): a wrong password on POST
// /api/session is a uniform 401 "invalid credentials" AND writes a session.login_fail audit row. Red:
// the handler 200s or the audit row is missing.
func TestSession_WrongPassword_Uniform401(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "heidi", "pw-heidi", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := testAuthHandler(pool)

	rec := postSession(h, `{"username":"heidi","password":"wrong"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d (%s)", rec.Code, rec.Body)
	}
	if got := jsonBody(t, rec)["code"]; got != "unauthorized" {
		t.Fatalf("code = %v, want unauthorized", got)
	}
	if sessionCookieFrom(rec) != nil {
		t.Fatal("wrong password set a session cookie")
	}
	if n := sessionAuditCount(t, pool, "session.login_fail", "heidi"); n != 1 {
		t.Fatalf("session.login_fail audit rows = %d, want 1", n)
	}
}

// TestSession_LoginSuccess_SetsCookieRowAudit: a correct login is 200, sets a hardened ppk_sid cookie
// (HttpOnly, Secure, SameSite=Strict, Max-Age>0), inserts an admin_sessions row, and writes a
// session.login audit row. Red: no cookie / no row / weak cookie flags / no audit.
func TestSession_LoginSuccess_SetsCookieRowAudit(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "ivan", "pw-ivan", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := testAuthHandler(pool)

	rec := postSession(h, `{"username":"ivan","password":"pw-ivan"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	c := sessionCookieFrom(rec)
	if c == nil {
		t.Fatal("login set no ppk_sid cookie")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 || c.Value == "" {
		t.Fatalf("cookie not hardened: %+v", c)
	}
	// The session row exists and is resolvable by the cookie secret.
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&rows); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if rows != 1 {
		t.Fatalf("admin_sessions rows = %d, want 1", rows)
	}
	if n := sessionAuditCount(t, pool, "session.login", "ivan"); n != 1 {
		t.Fatalf("session.login audit rows = %d, want 1", n)
	}

	// A mutating cookie-authed request WITHOUT the CSRF header is a 403 csrf_required (design §4.1,
	// second layer beside SameSite=Strict) — the session row stays.
	noHdr := httptest.NewRequest("DELETE", "/api/session", nil)
	noHdr.AddCookie(c)
	nrec := httptest.NewRecorder()
	h.ServeHTTP(nrec, noHdr)
	if nrec.Code != http.StatusForbidden {
		t.Fatalf("logout without X-Requested-With: want 403, got %d (%s)", nrec.Code, nrec.Body)
	}
	if got := jsonBody(t, nrec)["code"]; got != "csrf_required" {
		t.Fatalf("code = %v, want csrf_required", got)
	}

	// With the header, the cookie authenticates DELETE /api/session (logout): row gone, cookie
	// cleared (Max-Age<=0).
	del := httptest.NewRequest("DELETE", "/api/session", nil)
	del.AddCookie(c)
	del.Header.Set("X-Requested-With", "picpak")
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, del)
	if drec.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d (%s)", drec.Code, drec.Body)
	}
	cleared := sessionCookieFrom(drec)
	if cleared == nil || cleared.MaxAge > 0 {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&rows); err != nil {
		t.Fatalf("count sessions after logout: %v", err)
	}
	if rows != 0 {
		t.Fatalf("admin_sessions rows after logout = %d, want 0", rows)
	}
}

// TestSession_DummyVerify_UniformLatency is the W3 negative probe (2): an unknown username costs
// (statistically) the same wall-clock time as a wrong password — both run a full argon2 verify. Red:
// the unknown-user path short-circuits before argon2 and its latency collapses.
func TestSession_DummyVerify_UniformLatency(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "judy", "pw-judy", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := testAuthHandler(pool)

	const n = 5
	median := func(user, pass string) time.Duration {
		ds := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			if rec := postSession(h, `{"username":"`+user+`","password":"`+pass+`"}`, nil); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s/%s: want 401, got %d", user, pass, rec.Code)
			}
			ds = append(ds, time.Since(start))
		}
		for i := 0; i < len(ds); i++ {
			for j := i + 1; j < len(ds); j++ {
				if ds[j] < ds[i] {
					ds[i], ds[j] = ds[j], ds[i]
				}
			}
		}
		return ds[len(ds)/2]
	}
	wrongPw := median("judy", "nope")
	unknown := median("nobody", "nope")
	if unknown < wrongPw/2 {
		t.Fatalf("dummy-verify timing oracle: unknown-user median %v << wrong-password median %v", unknown, wrongPw)
	}
	_ = ctx
}

// TestSession_LoginFlood_RateLimited is the W4 flood gate on the REAL POST /api/session, wired exactly as
// main.go wires it (WithRequestID(IPRateLimit(mux))). After N wrong-password attempts from one IP the
// next is 429 WITH Retry-After, and — the load-bearing assertion — it is blocked BEFORE the login handler
// runs: the handler writes a session.login_fail audit row on every failed verify, so the blocked request
// leaving the audit count at N (not N+1) proves it never reached the argon2 verify. Red (no IPRateLimit
// wrap): the sixth attempt is another 401 with a sixth audit row, no 429.
func TestSession_LoginFlood_RateLimited(t *testing.T) {
	pool := dbPool(t)
	resetAuthTables(t, pool)
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "mallory", "pw-mallory", false); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Pin the pre-auth IP limit small and deterministic; restore the defaults afterwards.
	os.Setenv("ADMIN_RATELIMIT_IP_PER_MIN", "5")
	adminhttp.ConfigureRateLimitsFromEnv()
	defer func() { os.Unsetenv("ADMIN_RATELIMIT_IP_PER_MIN"); adminhttp.ConfigureRateLimitsFromEnv() }()

	mux := http.NewServeMux()
	registerAuthRoutes(mux, pool)
	h := adminhttp.WithRequestID(adminhttp.IPRateLimit(mux)) // exactly main.go's wiring

	for i := 1; i <= 5; i++ {
		if rec := postSession(h, `{"username":"mallory","password":"wrong"}`, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d (%s)", i, rec.Code, rec.Body)
		}
	}
	rec := postSession(h, `{"username":"mallory","password":"wrong"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: want 429, got %d (%s)", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("login 429 without Retry-After header")
	}
	if n := sessionAuditCount(t, pool, "session.login_fail", "mallory"); n != 5 {
		t.Fatalf("session.login_fail rows = %d, want 5 — the 6th (429) must be blocked before the login handler (argon2)", n)
	}
}
