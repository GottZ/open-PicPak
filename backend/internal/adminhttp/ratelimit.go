// Rate limiting (design §4.4, A28 W4). Two brakes, both in-memory (one process, one host-port — no new
// infra) and both hard-capped in memory so a source-diverse flood cannot itself exhaust the admin RAM
// after the SSO removal makes admin public:
//
//   - Post-auth PRINCIPAL bucket (the primary brake): one bucket per resolved Principal (kind+id), split
//     into a write and a read family (design §4.4: reads großzügiger). The Principal is non-spoofable —
//     it comes from a verified credential, not client input — so this is the reliable flood defence. It
//     is charged on every authenticated request, inside Auth (admitPrincipal), so ONE principal over its
//     limit is 429'd while a second principal is untouched.
//   - Pre-auth IP bucket (the brute-force brake): one bucket per non-spoofable client IP (RemoteIP =
//     X-Real-IP on the public listener, never X-Forwarded-For). IPRateLimit wraps the whole mux: an
//     empty bucket is 429'd BEFORE the handler runs, so a login/credential flood never reaches the
//     argon2 verify it is trying to exhaust; a token is charged only when the outcome is a 401 (a failed
//     credential, or a wrong-password POST /api/session — its argon2 sits behind this gate, so the login
//     brute-force surface is covered without a special case).
//
// Both maps are hard-capped LRUs (same shape as basicCache): the least-recently-used key is evicted over
// the cap, so an IP- or principal-diverse flood is bounded in memory, dimensioned at the ADVERSARIAL
// source cardinality (a distributed scan), not the device count (design §4.4 / §6). Limits/windows/caps
// are Policy=Data (ADMIN_RATELIMIT_*); the bucket mechanism is code.
package adminhttp

import (
	"container/list"
	"context"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ---- token-bucket LRU limiter ----

// tokenBucket is one key's refilling allowance: tokens in [0, burst], recomputed lazily on access from
// the time since lastRefill. elem is its node in the owner's LRU list.
type tokenBucket struct {
	tokens     float64
	lastRefill time.Time
	elem       *list.Element
}

// rateLimiter is a hard-capped LRU of token buckets: `rate` tokens/second with a burst of `rate*window`
// (== the per-window limit), at most `max` distinct keys resident. The cap is the memory bound — over it
// the least-recently-used key is evicted, so a key-diverse flood never grows the map without limit.
type rateLimiter struct {
	mu        sync.Mutex
	rate      float64       // tokens per second
	burst     float64       // bucket capacity == the per-window limit
	window    time.Duration // nominal refill window (for Retry-After when rate is unknown)
	max       int
	buckets   map[string]*tokenBucket
	lru       *list.List // Value = map key; front = most recently used
	evictions atomic.Int64
}

func newRateLimiter(limit int, window time.Duration, max int) *rateLimiter {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	if max < 1 {
		max = 1
	}
	return &rateLimiter{
		rate:    float64(limit) / window.Seconds(),
		burst:   float64(limit),
		window:  window,
		max:     max,
		buckets: make(map[string]*tokenBucket),
		lru:     list.New(),
	}
}

// bucketLocked returns key's bucket, refilled to now, creating it (a full bucket) and evicting the LRU
// over the cap as needed. Caller holds mu.
func (rl *rateLimiter) bucketLocked(key string) *tokenBucket {
	now := time.Now()
	if b, ok := rl.buckets[key]; ok {
		if b.tokens += now.Sub(b.lastRefill).Seconds() * rl.rate; b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.lastRefill = now
		rl.lru.MoveToFront(b.elem)
		return b
	}
	b := &tokenBucket{tokens: rl.burst, lastRefill: now}
	b.elem = rl.lru.PushFront(key)
	rl.buckets[key] = b
	for rl.lru.Len() > rl.max {
		back := rl.lru.Back()
		if back == nil {
			break
		}
		rl.lru.Remove(back)
		delete(rl.buckets, back.Value.(string))
		rl.evictions.Add(1)
	}
	return b
}

// allow consumes one token from key if a whole token is available, returning true; otherwise false (the
// caller answers 429). A brand-new key starts full. Used by the post-auth principal gate.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.bucketLocked(key)
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// peek reports whether key currently has a token WITHOUT consuming it: the pre-auth IP gate blocks an
// empty bucket before running the handler, then charges only a chargeable (401) outcome via consume.
func (rl *rateLimiter) peek(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.bucketLocked(key).tokens >= 1
}

// consume charges one token against key (floored at zero).
func (rl *rateLimiter) consume(key string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.bucketLocked(key)
	if b.tokens--; b.tokens < 0 {
		b.tokens = 0
	}
}

// retryAfter is the whole-second wait until key holds a token again (>=1), for the Retry-After header.
func (rl *rateLimiter) retryAfter(key string) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.bucketLocked(key)
	if b.tokens >= 1 {
		return 1
	}
	if rl.rate <= 0 {
		return int(rl.window.Seconds())
	}
	secs := (1 - b.tokens) / rl.rate
	n := int(secs)
	if float64(n) < secs {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

// len is the number of resident keys (for the eviction-bound probe).
func (rl *rateLimiter) len() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.buckets)
}

// ---- policy (Policy=Data, design §4.4) ----

var (
	ipLimiter      *rateLimiter // pre-auth per-IP brute-force brake
	principalWrite *rateLimiter // post-auth per-principal brake, mutating methods
	principalRead  *rateLimiter // post-auth per-principal brake, safe methods (großzügiger)
)

func init() { ConfigureRateLimitsFromEnv() }

// ConfigureRateLimitsFromEnv (re)builds the limiters from the ADMIN_RATELIMIT_* environment. Called once
// at package load; call again after changing the environment (an ops reload, or a test pinning small
// limits). Defaults come from design §4.4: 10 pre-auth attempts / IP / minute, 60 mutating + 600 read
// requests / principal / minute. The map caps are sized at the adversarial source cardinality (a
// distributed scan), NOT the device count — devices never speak admin (§6). Not safe to call
// concurrently with live traffic.
func ConfigureRateLimitsFromEnv() {
	principalMax := envIntOr("ADMIN_RATELIMIT_PRINCIPAL_MAX", 16384)
	ipLimiter = newRateLimiter(envIntOr("ADMIN_RATELIMIT_IP_PER_MIN", 10), time.Minute, envIntOr("ADMIN_RATELIMIT_IP_MAX", 65536))
	principalWrite = newRateLimiter(envIntOr("ADMIN_RATELIMIT_WRITE_PER_MIN", 60), time.Minute, principalMax)
	principalRead = newRateLimiter(envIntOr("ADMIN_RATELIMIT_READ_PER_MIN", 600), time.Minute, principalMax)
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// principalLimiterFor picks the read or write family by method (design §4.4: reads großzügiger). Read and
// write allowances are independent per principal (separate maps, same kind+id key).
func principalLimiterFor(method string) *rateLimiter {
	if csrfSafeMethod(method) {
		return principalRead
	}
	return principalWrite
}

// ---- the two gates ----

// statusRecorder captures the response status so IPRateLimit can charge the IP bucket only on a 401.
// It Unwrap()s to the wrapped writer: IPRateLimit sits around the WHOLE admin mux, so downstream
// handlers that need optional ResponseWriter capabilities (the SSE stream's per-frame
// http.ResponseController.Flush in cmd/admin/events.go) must be able to walk through it — the
// ResponseController discovers Flusher/Hijacker/deadlines via the Unwrap chain, and without this method
// the SSE stream would silently buffer until the fronting proxy times it out (lead-review finding, W4).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// Unwrap exposes the wrapped ResponseWriter to http.ResponseController's capability walk.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *statusRecorder) WriteHeader(code int) {
	if !rec.wrote {
		rec.status, rec.wrote = code, true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.status, rec.wrote = http.StatusOK, true
	}
	return rec.ResponseWriter.Write(b)
}

// IPRateLimit is the pre-auth IP brute-force brake (design §4.4). Wrap the whole admin mux with it: an IP
// whose bucket is empty is answered 429 (+ Retry-After) BEFORE next runs — so a login/credential flood is
// stopped before the argon2 verify it aims to exhaust — and a token is charged only when the outcome is a
// 401. POST /api/session's argon2 sits behind this gate, so a wrong-password login flood (each a 401) is
// covered without a special case. The IP is the non-spoofable RemoteIP (X-Real-IP on the public listener,
// NEVER X-Forwarded-For), so an attacker cannot rotate a header to escape the per-IP bucket. A 2xx or a
// scope-denied 403 charges nothing, so a legitimate client never rate-limits itself.
func IPRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := RemoteIP(r)
		if !ipLimiter.peek(ip) {
			w.Header().Set("Retry-After", strconv.Itoa(ipLimiter.retryAfter(ip)))
			WriteErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many attempts; retry shortly")
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusUnauthorized {
			ipLimiter.consume(ip)
		}
	})
}

// admitPrincipal applies the post-auth per-principal rate limit (design §4.4, the PRIMARY brake), then
// dispatches with the Principal in ctx. Over the per-principal limit → 429 rate_limited + Retry-After,
// BEFORE the handler runs, so a flood by one authenticated client cannot drive the handler cost. The key
// is kind+id, so each token / session is limited independently — one principal over the limit never
// throttles another. Called from Auth at every successful carrier resolution.
func admitPrincipal(w http.ResponseWriter, r *http.Request, next http.Handler, ctx context.Context, pr Principal) {
	limiter := principalLimiterFor(r.Method)
	key := string(pr.Kind) + ":" + pr.ID
	if !limiter.allow(key) {
		w.Header().Set("Retry-After", strconv.Itoa(limiter.retryAfter(key)))
		WriteErr(w, r, http.StatusTooManyRequests, "rate_limited", "request rate exceeded; retry shortly")
		return
	}
	next.ServeHTTP(w, r.WithContext(setPrincipal(ctx, pr)))
}
