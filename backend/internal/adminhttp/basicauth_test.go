package adminhttp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminuser"
)

// basicReq drives one Basic-auth request through the handler and returns the recorder.
func basicReq(t *testing.T, h http.Handler, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/probe", nil)
	r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func truncateAudit(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE admin_audit RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate audit: %v", err)
	}
}

func auditCount(t *testing.T, pool *pgxpool.Pool, action, target string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_audit WHERE action=$1 AND target=$2`, action, target).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// TestBasic_Success_And_WrongPassword_Uniform401 is the delta-4 negative probe (1): a correct Basic
// credential authenticates to a basic Principal (IsAdmin from admin_users); a WRONG password is a
// uniform 401 with NO WWW-Authenticate header (a challenge would make Basic ambient/CSRF-able) and a
// synchronous basic.login_fail audit row. Red: the handler 200s, sets WWW-Authenticate, or skips audit.
func TestBasic_Success_And_WrongPassword_Uniform401(t *testing.T) {
	pool := dbPool(t)
	truncateAudit(t, pool)
	globalBasicCache = newBasicCache(60*time.Second, 1024) // fresh cache, isolate from other tests
	ctx := context.Background()

	if _, err := adminuser.Create(ctx, pool, "carol", "correct horse", true); err != nil {
		t.Fatalf("create user: %v", err)
	}
	var seen Principal
	h := probeChain(pool, nil, &seen)

	// Correct credential → 200, basic Principal, admin flag carried, implicit full scopes.
	if rec := basicReq(t, h, "carol", "correct horse"); rec.Code != http.StatusOK {
		t.Fatalf("correct basic: want 200, got %d (%s)", rec.Code, rec.Body)
	}
	if seen.Kind != KindBasic || !seen.IsAdmin || !seen.HasScope(ScopeImageWrite) || seen.Label != "carol" {
		t.Fatalf("basic principal wrong: %+v", seen)
	}

	// Wrong password → uniform 401, NO WWW-Authenticate, synchronous basic.login_fail audit.
	rec := basicReq(t, h, "carol", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", rec.Code)
	}
	if ch := rec.Header().Get("WWW-Authenticate"); ch != "" {
		t.Fatalf("wrong password set WWW-Authenticate=%q — load-bearing: must NEVER be set", ch)
	}
	if got := decode(t, rec.Body.Bytes())["code"]; got != "unauthorized" {
		t.Fatalf("wrong password code = %v, want unauthorized", got)
	}
	if n := auditCount(t, pool, "basic.login_fail", "carol"); n != 1 {
		t.Fatalf("basic.login_fail audit rows = %d, want 1", n)
	}
}

// TestBasic_DisabledUser_401 is the delta-4 negative probe (3): a disabled account fails Basic verify
// even with the correct password. Red: a disabled account still authenticates.
func TestBasic_DisabledUser_401(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(60*time.Second, 1024)
	ctx := context.Background()

	u, err := adminuser.Create(ctx, pool, "dave", "pw-dave", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := adminuser.SetDisabled(ctx, pool, u.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	h := probeChain(pool, nil, nil)
	if rec := basicReq(t, h, "dave", "pw-dave"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled basic: want 401, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestBasic_RequireAdmin is the delta-4 negative probe (4): a non-admin Basic Principal on a
// RequireAdmin route is 403; an admin Basic Principal clears the gate. Red: a non-admin reaches an
// admin route.
func TestBasic_RequireAdmin(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(60*time.Second, 1024)
	ctx := context.Background()

	if _, err := adminuser.Create(ctx, pool, "ro-basic", "pw-ro", false); err != nil {
		t.Fatalf("create ro: %v", err)
	}
	if _, err := adminuser.Create(ctx, pool, "admin-basic", "pw-admin", true); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h := probeChain(pool, RequireAdmin, nil)

	if rec := basicReq(t, h, "ro-basic", "pw-ro"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin basic on RequireAdmin: want 403, got %d (%s)", rec.Code, rec.Body)
	}
	if rec := basicReq(t, h, "admin-basic", "pw-admin"); rec.Code != http.StatusOK {
		t.Fatalf("admin basic on RequireAdmin: want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestBasic_DummyVerify_UniformLatency is the delta-4 negative probe (2): an unknown username costs
// (statistically) the same wall-clock time as a wrong password, because both run a full argon2 verify
// (adminuser.Verify runs a dummy hash for the unknown user). Red: the unknown-user path short-circuits
// before argon2 → its latency collapses to near-zero and the ratio falls below the floor.
func TestBasic_DummyVerify_UniformLatency(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(0, 1) // cache OFF so every request re-runs the verify
	ctx := context.Background()

	if _, err := adminuser.Create(ctx, pool, "erin", "pw-erin", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := probeChain(pool, nil, nil)

	const n = 5
	median := func(label, user, pass string) time.Duration {
		ds := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			if rec := basicReq(t, h, user, pass); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: want 401, got %d", label, rec.Code)
			}
			ds = append(ds, time.Since(start))
		}
		// simple median of 5
		for i := 0; i < len(ds); i++ {
			for j := i + 1; j < len(ds); j++ {
				if ds[j] < ds[i] {
					ds[i], ds[j] = ds[j], ds[i]
				}
			}
		}
		return ds[len(ds)/2]
	}

	wrongPw := median("wrong-password", "erin", "nope")
	unknown := median("unknown-user", "nobody", "nope")
	// Both must pay argon2; the unknown-user path must be at least half the wrong-password latency.
	if unknown < wrongPw/2 {
		t.Fatalf("dummy-verify timing oracle: unknown-user median %v << wrong-password median %v", unknown, wrongPw)
	}
}

// TestBasic_SemaphoreFlood is the delta-4 negative probe (5): a parallel flood of distinct (random)
// credentials saturates the global argon2 semaphore, so surplus requests are rejected with 429 instead
// of each spawning a ~64 MiB verify. Red (no semaphore): every request runs argon2 concurrently and
// none returns 429. None of the random credentials is valid, so none is 200.
func TestBasic_SemaphoreFlood(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(0, 1) // cache OFF — force the argon2 path for every request
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "frank", "pw-frank", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := probeChain(pool, nil, nil)

	// Pin the semaphore small and deterministic for the probe; restore afterwards.
	saved := argon2Sem
	argon2Sem = make(chan struct{}, 2)
	defer func() { argon2Sem = saved }()

	const flood = 24
	codes := make([]int, flood)
	var wg sync.WaitGroup
	for i := 0; i < flood; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// distinct password per goroutine → all miss the (disabled) cache and all fail verify
			codes[i] = basicReq(t, h, "frank", "wrong-"+string(rune('a'+i))).Code
		}(i)
	}
	wg.Wait()

	var got429, got200, got401 int
	for _, c := range codes {
		switch c {
		case http.StatusTooManyRequests:
			got429++
		case http.StatusOK:
			got200++
		case http.StatusUnauthorized:
			got401++
		}
	}
	if got200 != 0 {
		t.Fatalf("flood: %d requests authenticated with random passwords, want 0", got200)
	}
	if got429 == 0 {
		t.Fatalf("flood: no 429 — the argon2 semaphore did not engage (RSS uncapped). 401=%d", got401)
	}
	if got429+got401 != flood {
		t.Fatalf("flood: accounted %d of %d (429=%d 401=%d)", got429+got401, flood, got429, got401)
	}
	_ = ctx
}

// TestBasic_PositiveCache is the delta-4 negative probe (cache): a second identical Basic request
// within the TTL is served from the positive cache WITHOUT a second argon2 verify (the miss counter
// does not advance); a distinct credential still forces a verify. Red (no cache): the counter advances
// on every request.
func TestBasic_PositiveCache(t *testing.T) {
	pool := dbPool(t)
	globalBasicCache = newBasicCache(60*time.Second, 1024) // TTL on
	ctx := context.Background()
	if _, err := adminuser.Create(ctx, pool, "grace", "pw-grace", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := probeChain(pool, nil, nil)

	before := argon2Verifies.Load()
	if rec := basicReq(t, h, "grace", "pw-grace"); rec.Code != http.StatusOK {
		t.Fatalf("first request: want 200, got %d", rec.Code)
	}
	afterFirst := argon2Verifies.Load()
	if afterFirst != before+1 {
		t.Fatalf("first request verify count delta = %d, want 1", afterFirst-before)
	}
	// Second identical request within TTL → cache hit, NO new argon2 verify.
	if rec := basicReq(t, h, "grace", "pw-grace"); rec.Code != http.StatusOK {
		t.Fatalf("cached request: want 200, got %d", rec.Code)
	}
	if got := argon2Verifies.Load(); got != afterFirst {
		t.Fatalf("cached request ran argon2 (count %d→%d) — positive cache did not short-circuit", afterFirst, got)
	}
	// A different credential still misses the cache and runs a verify.
	if rec := basicReq(t, h, "grace", "different"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("distinct wrong credential: want 401, got %d", rec.Code)
	}
	if got := argon2Verifies.Load(); got != afterFirst+1 {
		t.Fatalf("distinct credential verify count delta = %d, want 1", got-afterFirst)
	}
	_ = ctx
}
