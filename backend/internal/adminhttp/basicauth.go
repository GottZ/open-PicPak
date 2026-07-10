// Basic-auth carrier plumbing (design delta-4): the fourth credential carrier, a global argon2
// semaphore shared with POST /api/session, a short-lived positive cache, and the non-spoofable client
// IP reader the audit trail records. argon2id costs ~64 MiB + ~50-100 ms per verify, and after the SSO
// removal admin is public — so an unbounded number of concurrent verifies (Basic re-auth per request
// OR a login flood) is a RAM/CPU exhaustion vector. The semaphore caps concurrency and the cache
// removes the per-request argon2 cost for a client that repeats one credential.
package adminhttp

import (
	"container/list"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminaudit"
	"github.com/open-picpak/backend/internal/adminuser"
)

// ---- global argon2 semaphore (design delta-4) ----

// defaultArgon2Parallel is the default number of concurrent argon2 verifies. Small on purpose: each
// verify pins ~64 MiB, so 3 caps the argon2 working set at ~192 MiB regardless of load.
const defaultArgon2Parallel = 3

// argon2Sem is a non-blocking counting semaphore. An overflow is REJECTED with 429, never queued —
// queuing would let the backlog (and its pinned 64 MiB buffers) grow without bound, which is the very
// exhaustion the semaphore exists to prevent. It is reassignable in-package so tests can resize it.
var argon2Sem = make(chan struct{}, argon2ParallelFromEnv())

func argon2ParallelFromEnv() int {
	if v := os.Getenv("ADMIN_ARGON2_PARALLEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultArgon2Parallel
}

// TryAcquireArgon2 reserves one of the global argon2 slots without blocking, returning false when all
// slots are busy (the caller then rejects with 429 — never queue). Pair every true with ReleaseArgon2.
// Shared by the Basic carrier here AND cmd/admin's POST /api/session so the two verify paths cannot
// together exceed the RAM cap.
func TryAcquireArgon2() bool {
	select {
	case argon2Sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseArgon2 frees a slot reserved by TryAcquireArgon2.
func ReleaseArgon2() { <-argon2Sem }

// ---- Basic positive cache (design delta-4) ----

// argon2Verifies counts full Basic argon2 verifies (cache MISSES). A cache HIT does not increment it,
// so a test can prove the positive cache short-circuits a repeated credential. Process-global, monotonic.
var argon2Verifies atomic.Int64

// basicCacheEntry is one cached success. Only successful verifies are cached — a wrong password always
// re-runs the full verify (no negative cache to poison), and a password change or account disable takes
// effect within one TTL.
type basicCacheEntry struct {
	digest  [32]byte
	userID  int64
	isAdmin bool
	expires time.Time
	elem    *list.Element
}

// basicCache is a hard-capped LRU keyed by sha256(user \x00 pass). The cap bounds memory so an
// adversarial spray of distinct credentials cannot grow the map without bound; the stored digest is
// re-compared in constant time on every hit (design delta-4).
type basicCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*basicCacheEntry
	lru     *list.List // Value = map key; front = most recently used
}

func newBasicCache(ttl time.Duration, max int) *basicCache {
	if max < 1 {
		max = 1
	}
	return &basicCache{ttl: ttl, max: max, entries: make(map[string]*basicCacheEntry), lru: list.New()}
}

var globalBasicCache = newBasicCache(basicCacheTTLFromEnv(), basicCacheMaxFromEnv())

// basicCacheTTLFromEnv reads ADMIN_BASIC_CACHE_TTL (a duration, e.g. "60s"); unset defaults to 60s and
// an explicit 0 (or a non-positive value) DISABLES the cache — every Basic request then runs argon2.
func basicCacheTTLFromEnv() time.Duration {
	v := os.Getenv("ADMIN_BASIC_CACHE_TTL")
	if v == "" {
		return 60 * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 60 * time.Second
	}
	return d // <=0 disables (checked in get/put)
}

func basicCacheMaxFromEnv() int {
	if v := os.Getenv("ADMIN_BASIC_CACHE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1024
}

func basicDigest(user, pass string) [32]byte {
	h := sha256.New()
	h.Write([]byte(user))
	h.Write([]byte{0})
	h.Write([]byte(pass))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// get returns a live cached entry for the credential, or (_, false) on a miss/expiry/disabled cache.
func (c *basicCache) get(user, pass string) (basicCacheEntry, bool) {
	if c.ttl <= 0 {
		return basicCacheEntry{}, false
	}
	d := basicDigest(user, pass)
	key := string(d[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return basicCacheEntry{}, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(key, e)
		return basicCacheEntry{}, false
	}
	if subtle.ConstantTimeCompare(e.digest[:], d[:]) != 1 {
		return basicCacheEntry{}, false
	}
	c.lru.MoveToFront(e.elem)
	return *e, true
}

// put stores (or refreshes) a successful verify, evicting the least-recently-used entry over the cap.
func (c *basicCache) put(user, pass string, userID int64, isAdmin bool) {
	if c.ttl <= 0 {
		return
	}
	d := basicDigest(user, pass)
	key := string(d[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.userID, e.isAdmin, e.expires = userID, isAdmin, time.Now().Add(c.ttl)
		c.lru.MoveToFront(e.elem)
		return
	}
	e := &basicCacheEntry{digest: d, userID: userID, isAdmin: isAdmin, expires: time.Now().Add(c.ttl)}
	e.elem = c.lru.PushFront(key)
	c.entries[key] = e
	for c.lru.Len() > c.max {
		if back := c.lru.Back(); back != nil {
			c.removeLocked(back.Value.(string), nil)
		}
	}
}

func (c *basicCache) removeLocked(key string, e *basicCacheEntry) {
	if e == nil {
		e = c.entries[key]
	}
	if e == nil {
		return
	}
	c.lru.Remove(e.elem)
	delete(c.entries, key)
}

// ---- Basic carrier resolution ----

// resolveBasic authenticates HTTP Basic credentials against admin_users, IDENTICALLY to POST
// /api/session (dummy-verify for an unknown user, uniform failure). A positive-cache hit skips argon2;
// a miss runs the semaphore-gated verify. It returns the resolved Principal and an HTTP status: 200
// authenticated, 401 for any failure (unknown user / wrong password / disabled — uniform, no
// enumeration signal), 429 when the argon2 semaphore is saturated, 500 on a store error. A failed
// verify writes basic.login_fail SYNCHRONOUSLY (design §4.6); successes are not audited per request.
func resolveBasic(ctx context.Context, db *pgxpool.Pool, user, pass, remoteIP string) (Principal, int) {
	if e, ok := globalBasicCache.get(user, pass); ok {
		return basicPrincipal(user, e.userID, e.isAdmin), http.StatusOK
	}
	if !TryAcquireArgon2() {
		return Principal{}, http.StatusTooManyRequests
	}
	defer ReleaseArgon2()

	argon2Verifies.Add(1)
	u, ok, err := adminuser.Verify(ctx, db, user, pass) // runs a dummy argon2 for an unknown user
	if err != nil {
		return Principal{}, http.StatusInternalServerError
	}
	if !ok {
		_ = adminaudit.Write(ctx, db, adminaudit.Entry{
			ActorKind: "basic", Action: "basic.login_fail", Target: user, RemoteIP: remoteIP, OK: false,
		})
		return Principal{}, http.StatusUnauthorized
	}
	globalBasicCache.put(user, pass, u.ID, u.IsAdmin)
	return basicPrincipal(user, u.ID, u.IsAdmin), http.StatusOK
}

// basicPrincipal builds the Basic identity: implicit full image scopes (a human), IsAdmin from the
// account (design delta-4 §Autonome Festlegungen — Basic carries IsAdmin like a session).
func basicPrincipal(user string, userID int64, isAdmin bool) Principal {
	return Principal{
		Kind:    KindBasic,
		ID:      strconv.FormatInt(userID, 10),
		Label:   user,
		Scopes:  implicitFullScopes(),
		IsAdmin: isAdmin,
	}
}

// hasBasicScheme reports whether the Authorization header carries the Basic scheme. Once seen, Auth
// commits to the Basic path (a malformed Basic header is a 401, not a fall-through to the cookie).
func hasBasicScheme(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const pfx = "Basic "
	return len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx)
}

// ---- client IP (design §4.4) ----

// RemoteIP returns the request's client IP from the non-spoofable source fixed in design §4.4: on the
// public-proxied listener the reverse proxy's X-Real-IP (the real host-net client, which the proxy
// sets and a client cannot append), on the loopback listener (or an untagged one, e.g. tests) the
// tunnel-local RemoteAddr. This is the source the audit trail records; W4 builds the rate-limit
// buckets on the same reader. X-Forwarded-For is deliberately NEVER read — its leftmost value is
// client-settable, so trusting it would let an attacker rotate it and defeat any per-IP limit.
func RemoteIP(r *http.Request) string {
	if o, ok := ListenerOriginFrom(r.Context()); ok && o == OriginPublic {
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
			return xrip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
