package adminhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminsession"
	"github.com/open-picpak/backend/internal/adminuser"
	"github.com/open-picpak/backend/internal/apitoken"
)

// DB-gated carrier tests — skipped unless TEST_DATABASE_URL is set (an ephemeral postgres with
// migrations 0001..0011 + 0014 applied). Mirrors the W1 store test pattern. They drive the real Auth
// middleware end-to-end over httptest, so the carrier dispatch, uniform 401 and scope gate are
// exercised against live rows, not fakes.
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB carrier tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE api_tokens, admin_sessions, admin_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	resetRateLimitersForTest() // fresh, generous buckets so W1–W3 carrier tests never trip the W4 gates
	return pool
}

// echoScope is a handler that 200s and records the resolved Principal for assertions.
func probeChain(pool *pgxpool.Pool, gate func(http.Handler) http.Handler, seen *Principal) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFrom(r.Context()); ok && seen != nil {
			*seen = p
		}
		w.WriteHeader(http.StatusOK)
	})
	h := http.Handler(inner)
	if gate != nil {
		h = gate(inner)
	}
	return Auth(pool)(h)
}

func req(t *testing.T, h http.Handler, auth, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/probe", nil)
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestAuth_PPKBearer_Uniform401 is probe (a)+(c): a wrong secret, an unknown token_id, a revoked and
// an expired token all resolve to the SAME 401 body — no enumeration signal — while the correct
// token authenticates as a bearer Principal carrying exactly its scopes.
func TestAuth_PPKBearer_Uniform401(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	var seen Principal
	h := probeChain(pool, nil, &seen)

	good, _, err := apitoken.Create(ctx, pool, "reader", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tokenID, secret, _ := apitoken.ParseToken(good)

	// Correct token → 200, bearer Principal, exact scopes, never admin.
	if rec := req(t, h, good, ""); rec.Code != http.StatusOK {
		t.Fatalf("correct token: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if seen.Kind != KindBearer || seen.IsAdmin || !seen.HasScope("image:read") || seen.HasScope("image:write") {
		t.Fatalf("bearer principal wrong: %+v", seen)
	}

	// The uniform-401 family: wrong secret (known id), unknown id, garbage, missing.
	forged := TokenPrefixWrong(tokenID, secret)
	unknown := apitoken.TokenPrefix + "deadbeefdead_" + secret
	bodies := map[string]string{}
	for name, tok := range map[string]string{"wrong-secret": forged, "unknown-id": unknown, "garbage": "not-a-token", "missing": ""} {
		rec := req(t, h, tok, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %d", name, rec.Code)
		}
		bodies[name] = rec.Body.String()
	}
	for name, b := range bodies {
		if b != bodies["wrong-secret"] {
			t.Fatalf("401 body for %q differs from wrong-secret (enumeration signal): %q vs %q", name, b, bodies["wrong-secret"])
		}
	}

	// Revoked token → 401 (probe c). Create a fresh one, then revoke.
	rev, tok, err := apitoken.Create(ctx, pool, "revoke-me", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if err := apitoken.Revoke(ctx, pool, tok.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rec := req(t, h, rev, ""); rec.Code != http.StatusUnauthorized || rec.Body.String() != bodies["wrong-secret"] {
		t.Fatalf("revoked: want uniform 401, got %d %q", rec.Code, rec.Body)
	}

	// Expired token → 401 (probe c).
	past := time.Now().Add(-time.Hour)
	exp, _, err := apitoken.Create(ctx, pool, "expired", []string{"image:read"}, &past, nil)
	if err != nil {
		t.Fatalf("create3: %v", err)
	}
	if rec := req(t, h, exp, ""); rec.Code != http.StatusUnauthorized || rec.Body.String() != bodies["wrong-secret"] {
		t.Fatalf("expired: want uniform 401, got %d %q", rec.Code, rec.Body)
	}
}

// TokenPrefixWrong builds a ppk_ token with a valid id but a corrupted last secret byte.
func TokenPrefixWrong(tokenID, secret string) string {
	last := byte('A')
	if secret[len(secret)-1] == 'A' {
		last = 'B'
	}
	return apitoken.TokenPrefix + tokenID + "_" + secret[:len(secret)-1] + string(last)
}

// TestAuth_ScopeGate is probe (b): a correct image:read token on a RequireScope("image:write") route
// is 403; the same token on an image:read route is 200.
func TestAuth_ScopeGate(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	good, _, err := apitoken.Create(ctx, pool, "reader", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	writeChain := probeChain(pool, RequireScope(ScopeImageWrite), nil)
	if rec := req(t, writeChain, good, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("image:read on write route: want 403, got %d (%s)", rec.Code, rec.Body)
	}
	if m := decode(t, req(t, writeChain, good, "").Body.Bytes()); m["code"] != "forbidden" {
		t.Fatalf("want code forbidden, got %v", m)
	}

	readChain := probeChain(pool, RequireScope(ScopeImageRead), nil)
	if rec := req(t, readChain, good, ""); rec.Code != http.StatusOK {
		t.Fatalf("image:read on read route: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestAuth_SessionCarrier: a live session cookie resolves to a session Principal (implicit full
// scopes, admin flag from the account); an unknown cookie is a uniform 401; a disabled account with
// a live session fails closed.
func TestAuth_SessionCarrier(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	var seen Principal
	h := probeChain(pool, nil, &seen)

	u, err := adminuser.Create(ctx, pool, "alice", "correct horse", true)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	secret, _, err := adminsession.Create(ctx, pool, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if rec := req(t, h, "", secret); rec.Code != http.StatusOK {
		t.Fatalf("valid session: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if seen.Kind != KindSession || !seen.IsAdmin || !seen.HasScope(ScopeImageWrite) || seen.Label != "alice" {
		t.Fatalf("session principal wrong: %+v", seen)
	}

	if rec := req(t, h, "", "bogus-cookie-secret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown cookie: want 401, got %d", rec.Code)
	}

	// Disabled account with a still-live session → fail closed.
	if err := adminuser.SetDisabled(ctx, pool, u.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if rec := req(t, h, "", secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled account: want 401, got %d", rec.Code)
	}
}
