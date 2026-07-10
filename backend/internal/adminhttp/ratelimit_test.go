package adminhttp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/open-picpak/backend/internal/adminuser"
	"github.com/open-picpak/backend/internal/apitoken"
)

// resetRateLimitersForTest reinstalls fresh, generous buckets so the W1–W3 carrier tests (which run
// through the same Auth path the W4 principal gate now sits on) never trip a limit. The W4 gate tests
// below pin their own small limiters AFTER this reset.
func resetRateLimitersForTest() {
	ipLimiter = newRateLimiter(1_000_000, time.Minute, 1<<20)
	principalWrite = newRateLimiter(1_000_000, time.Minute, 1<<20)
	principalRead = newRateLimiter(1_000_000, time.Minute, 1<<20)
}

// count401Stub returns a handler that models a failed-auth / failed-login endpoint: it 401s and counts
// how many times it actually RAN, so a probe can prove the pre-auth gate blocks the surplus BEFORE the
// handler (and thus before its argon2 verify) executes.
func count401Stub(ran *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran++
		WriteErr(w, r, http.StatusUnauthorized, "unauthorized", "invalid or missing credentials")
	})
}

// ipReq drives one request through an IP-gated handler on the PUBLIC listener (so RemoteIP reads the
// proxy-set X-Real-IP), with the given X-Real-IP and (spoofable) X-Forwarded-For.
func ipReq(h http.Handler, realIP, xff string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/session", nil)
	r = r.WithContext(WithListenerOrigin(r.Context(), OriginPublic))
	if realIP != "" {
		r.Header.Set("X-Real-IP", realIP)
	}
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestIPRateLimit_FloodBlocksBeforeHandler is the W4 flood probe (models the POST /api/session brute
// force): after N attempts from one IP the next is 429 WITH Retry-After, and — critically — is blocked
// BEFORE the handler runs, so a login flood never reaches the argon2 verify it targets. It also proves a
// 401-ending request ticks the IP bucket. Red (no gate): every attempt runs the handler as 401, no 429.
func TestIPRateLimit_FloodBlocksBeforeHandler(t *testing.T) {
	saved := ipLimiter
	defer func() { ipLimiter = saved }()
	ipLimiter = newRateLimiter(3, time.Minute, 1024)

	var ran int
	h := IPRateLimit(count401Stub(&ran))

	for i := 1; i <= 3; i++ {
		if rec := ipReq(h, "203.0.113.7", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, rec.Code)
		}
	}
	rec := ipReq(h, "203.0.113.7", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit attempt: want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After header")
	}
	if ran != 3 {
		t.Fatalf("handler ran %d times, want 3 — the 4th (429) must be blocked BEFORE the handler (its argon2)", ran)
	}
}

// TestIPRateLimit_XForwardedForDoesNotBypass is the W4 XFF-spoof probe. Holding the proxy-set X-Real-IP
// constant while rotating a spoofed X-Forwarded-For per request must NOT create fresh buckets — the limit
// binds to X-Real-IP, so a 429 still lands. Red: if RemoteIP trusted the client-appendable XFF, each
// rotated value would be a fresh bucket and no request would ever 429.
func TestIPRateLimit_XForwardedForDoesNotBypass(t *testing.T) {
	saved := ipLimiter
	defer func() { ipLimiter = saved }()

	ipLimiter = newRateLimiter(3, time.Minute, 1024)
	var ran int
	h := IPRateLimit(count401Stub(&ran))

	var got429 bool
	for i := 0; i < 6; i++ {
		if rec := ipReq(h, "198.51.100.9", "10.0.0."+strconv.Itoa(i)); rec.Code == http.StatusTooManyRequests {
			got429 = true
		}
	}
	if !got429 {
		t.Fatal("rotating X-Forwarded-For bypassed the per-IP limit — the gate must bind to X-Real-IP, not XFF")
	}

	// Contrast: rotating the TRUSTED source (X-Real-IP) itself DOES yield fresh buckets — proving the key
	// is exactly X-Real-IP. Six distinct real IPs, each well under the limit → never 429.
	ipLimiter = newRateLimiter(3, time.Minute, 1024)
	for i := 0; i < 6; i++ {
		if rec := ipReq(h, "198.51.100."+strconv.Itoa(i), "10.0.0.1"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("distinct X-Real-IP #%d was 429'd — the bucket key must be the per-real-client IP", i)
		}
	}
}

// TestIPRateLimit_DiverseFloodEvictionBounded is the W4 memory-cap probe: a 1000-distinct-IP flood must
// not grow the bucket map without bound — the hard-capped LRU evicts, keeping residency <= cap. The
// per-IP limit is set huge to isolate the MAP-cap behaviour from the per-IP brake. Red (uncapped map):
// residency climbs to 1000 and no eviction ever fires.
func TestIPRateLimit_DiverseFloodEvictionBounded(t *testing.T) {
	saved := ipLimiter
	defer func() { ipLimiter = saved }()

	const mapCap = 100
	ipLimiter = newRateLimiter(1_000_000, time.Minute, mapCap)
	h := IPRateLimit(count401Stub(new(int)))

	for i := 0; i < 1000; i++ {
		ipReq(h, fmt.Sprintf("10.0.%d.%d", i/256, i%256), "")
	}
	if n := ipLimiter.len(); n > mapCap {
		t.Fatalf("resident IP buckets = %d, want <= %d — the map cap did not bound an IP-diverse flood", n, mapCap)
	}
	if ev := ipLimiter.evictions.Load(); ev == 0 {
		t.Fatal("no evictions — the LRU cap never engaged under a 1000-IP flood")
	}
}

// TestIPRateLimit_PreservesFlush is the SSE-regression probe (lead review, W4): IPRateLimit wraps the
// WHOLE admin mux, so its statusRecorder sits under cmd/admin's SSE stream, whose per-frame
// http.ResponseController.Flush() discovers the Flusher by walking the Unwrap() chain. Without
// statusRecorder.Unwrap the walk dead-ends at the wrapper, Flush returns ErrNotSupported and the stream
// buffers (the fronting proxy then 504s it). httptest.ResponseRecorder implements Flush(), so the
// controller reaches it iff the wrapper unwraps — exactly the condition under probe. Red (no Unwrap):
// Flush errors "feature not supported" and the recorder never flushes. Green: Flush is nil and the
// flush reached the recorder.
func TestIPRateLimit_PreservesFlush(t *testing.T) {
	resetRateLimitersForTest()
	var flushErr error
	h := IPRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flushErr = http.NewResponseController(w).Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/events", nil))
	if flushErr != nil {
		t.Fatalf("Flush through IPRateLimit failed: %v — statusRecorder must Unwrap() to the flushable writer", flushErr)
	}
	if !rec.Flushed {
		t.Fatal("flush never reached the underlying recorder — the Unwrap chain is broken")
	}
}

// bearerReq drives one request with a ppk bearer through an Auth-gated handler.
func bearerReq(t *testing.T, h http.Handler, method, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/api/probe", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestPrincipalRateLimit_PerPrincipalIsolated is the W4 principal-brake probe (the PRIMARY brake): one
// authenticated principal driven over its per-principal write limit is 429'd WITH Retry-After, while a
// SECOND principal is untouched — the buckets are keyed by kind+id. Red (no gate): the first principal's
// 4th mutating request still 200s.
func TestPrincipalRateLimit_PerPrincipalIsolated(t *testing.T) {
	pool := dbPool(t) // also resets the limiters generously; we pin the write bucket small just below
	ctx := context.Background()

	savedW := principalWrite
	defer func() { principalWrite = savedW }()
	principalWrite = newRateLimiter(3, time.Minute, 1024)

	h := probeChain(pool, nil, nil) // Auth(pool)(200-stub)

	aTok, _, err := apitoken.Create(ctx, pool, "flooder", []string{"image:write"}, nil, nil)
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	bTok, _, err := apitoken.Create(ctx, pool, "bystander", []string{"image:write"}, nil, nil)
	if err != nil {
		t.Fatalf("create B: %v", err)
	}

	// Principal A: three mutating requests pass, the fourth is throttled.
	for i := 1; i <= 3; i++ {
		if rec := bearerReq(t, h, "POST", aTok); rec.Code != http.StatusOK {
			t.Fatalf("A request %d: want 200, got %d (%s)", i, rec.Code, rec.Body)
		}
	}
	rec := bearerReq(t, h, "POST", aTok)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("A over limit: want 429, got %d (%s)", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("principal 429 without Retry-After header")
	}

	// Principal B is unaffected by A's exhausted bucket.
	if rec := bearerReq(t, h, "POST", bTok); rec.Code != http.StatusOK {
		t.Fatalf("B (second principal): want 200, got %d (%s)", rec.Code, rec.Body)
	}
}

// TestIPRateLimit_BasicFailureTicksBucketBeforeArgon2 is the W4 probe (5): a wrong-password Basic flood
// (each a 401, no special case, delta-4) ticks the IP bucket, and the N-th repetition is 429 — blocked
// BEFORE the argon2 verify, proven by the argon2Verifies miss counter staying put on the throttled
// request. The Basic carrier runs through the real Auth chain behind IPRateLimit, exactly as production
// composes them. Red (no gate): the 4th wrong-Basic runs another argon2 verify and returns 401.
func TestIPRateLimit_BasicFailureTicksBucketBeforeArgon2(t *testing.T) {
	pool := dbPool(t) // resets limiters generously; we pin the IP bucket small just below
	ctx := context.Background()
	globalBasicCache = newBasicCache(0, 1) // cache OFF → every wrong password runs a real argon2 verify

	savedIP := ipLimiter
	defer func() { ipLimiter = savedIP }()
	ipLimiter = newRateLimiter(3, time.Minute, 1024)

	if _, err := adminuser.Create(ctx, pool, "peggy", "pw-peggy", false); err != nil {
		t.Fatalf("create: %v", err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := IPRateLimit(Auth(pool)(ok))

	wrongBasic := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/probe", nil)
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("peggy:wrong")))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	before := argon2Verifies.Load()
	for i := 1; i <= 3; i++ {
		if rec := wrongBasic(); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-basic %d: want 401, got %d", i, rec.Code)
		}
	}
	afterThree := argon2Verifies.Load()
	if afterThree != before+3 {
		t.Fatalf("argon2 verify delta after 3 wrong-basic = %d, want 3", afterThree-before)
	}
	rec := wrongBasic()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th wrong-basic: want 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("basic 429 without Retry-After header")
	}
	if got := argon2Verifies.Load(); got != afterThree {
		t.Fatalf("4th (429) ran argon2 (count %d→%d) — the IP gate must block a Basic-401 flood BEFORE the verify", afterThree, got)
	}
}
