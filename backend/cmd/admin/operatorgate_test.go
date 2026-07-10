package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/adminhttp"
	"github.com/open-picpak/backend/internal/apitoken"
	"github.com/open-picpak/backend/internal/operator"
)

// A28 W7 — operator_key public-exposure hardening (design §4.1 / §5 B8). These DB-gated tests drive
// the real adminhttp.Auth middleware over httptest, injecting the listener origin the way production
// BaseContext does, and prove: the operator (non-ppk_) bearer is honoured ONLY on the loopback
// listener; it is 401 on the public listener and on an untagged (unknown-provenance) request; an
// expired operator_key is 401 on every listener; and the other carriers (ppk_ / Basic) are unchanged
// on public. Skipped unless TEST_DATABASE_URL points at a schema with migration 0015 applied.

// authProbe drives Auth(pool) with the given listener origin and Authorization header, returning the
// resulting status. A 200 means the credential was admitted, anything else that it was rejected.
func authProbe(pool *pgxpool.Pool, origin adminhttp.ListenerOrigin, tagged bool, authz string) int {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := adminhttp.Auth(pool)(ok)
	r := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	if tagged {
		r = r.WithContext(adminhttp.WithListenerOrigin(r.Context(), origin))
	}
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func operatorKeyID(t *testing.T, pool *pgxpool.Pool, token string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM operator_keys WHERE token_hash = $1`, operator.HashToken(token)).Scan(&id); err != nil {
		t.Fatalf("lookup operator id: %v", err)
	}
	return id
}

// TestW7_OperatorListenerGate is the core B8 gate: the SAME live operator_key bearer is 401 on the
// public listener and 401 when the origin is untagged (fail closed), but 200 on the loopback listener.
func TestW7_OperatorListenerGate(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "op-tok", true)

	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer op-tok"); c != http.StatusOK {
		t.Fatalf("operator on loopback: want 200, got %d", c)
	}
	if c := authProbe(pool, adminhttp.OriginPublic, true, "Bearer op-tok"); c != http.StatusUnauthorized {
		t.Fatalf("operator on public: want 401, got %d", c)
	}
	if c := authProbe(pool, "", false, "Bearer op-tok"); c != http.StatusUnauthorized {
		t.Fatalf("operator on untagged origin (fail closed): want 401, got %d", c)
	}
}

// TestW7_ExpiredOperatorKey proves the expiry enforcement (deliverable 3): a key past expires_at is
// 401 even on the loopback listener; a NULL-expiry key (the incumbent-admin case) is unaffected.
func TestW7_ExpiredOperatorKey(t *testing.T) {
	pool := dbPool(t)
	seedOperator(t, pool, "live-tok", true)
	seedOperator(t, pool, "exp-tok", true)
	mustExec(t, pool, `UPDATE operator_keys SET expires_at = $1 WHERE token_hash = $2`,
		time.Now().Add(-time.Hour), operator.HashToken("exp-tok"))

	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer live-tok"); c != http.StatusOK {
		t.Fatalf("NULL-expiry key: want 200, got %d", c)
	}
	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer exp-tok"); c != http.StatusUnauthorized {
		t.Fatalf("expired key on loopback: want 401, got %d", c)
	}
}

// TestW7_RotateOperator proves the soft rotation (deliverable 4): the minted replacement authenticates
// immediately, the retired key stays valid within its grace window and is 401 once it elapses, and the
// replacement (no expiry) keeps working. The old key is retired by expiry, NOT hard-disabled.
func TestW7_RotateOperator(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	seedOperator(t, pool, "old-tok", true)
	oldID := operatorKeyID(t, pool, "old-tok")

	const grace = 250 * time.Millisecond
	rot, err := rotateOperatorKey(ctx, pool, oldID, grace)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !rot.IsAdmin || rot.Label != "test" || rot.NewID == oldID {
		t.Fatalf("rotation carried wrong identity: %+v (oldID=%d)", rot, oldID)
	}
	// The old key is retired via expiry, not a hard disable — disabled_at must still be NULL.
	var disabled *time.Time
	if err := pool.QueryRow(ctx, `SELECT disabled_at FROM operator_keys WHERE id = $1`, oldID).Scan(&disabled); err != nil {
		t.Fatalf("read old disabled_at: %v", err)
	}
	if disabled != nil {
		t.Fatalf("rotation hard-disabled the old key (want soft/expiry): disabled_at=%v", disabled)
	}

	// Immediately after rotation: replacement works, old key still valid within the grace window.
	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer "+rot.Token); c != http.StatusOK {
		t.Fatalf("new key immediately: want 200, got %d", c)
	}
	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer old-tok"); c != http.StatusOK {
		t.Fatalf("old key within grace: want 200, got %d", c)
	}

	// After the grace window elapses: old key is 401, replacement (no expiry) still 200.
	time.Sleep(grace + 300*time.Millisecond)
	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer old-tok"); c != http.StatusUnauthorized {
		t.Fatalf("old key after grace: want 401, got %d", c)
	}
	if c := authProbe(pool, adminhttp.OriginLoopback, true, "Bearer "+rot.Token); c != http.StatusOK {
		t.Fatalf("new key after grace: want 200, got %d", c)
	}
}

// TestW7_OtherCarriersUnchangedOnPublic is the non-regression guard: the ppk_ api-token carrier stays
// 200 on the PUBLIC listener (the gate only touches the operator fallback), and an unknown ppk_ token
// stays a uniform 401.
func TestW7_OtherCarriersUnchangedOnPublic(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	good, _, err := apitoken.Create(ctx, pool, "reader", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("create ppk: %v", err)
	}
	if c := authProbe(pool, adminhttp.OriginPublic, true, "Bearer "+good); c != http.StatusOK {
		t.Fatalf("ppk_ token on public: want 200 (unchanged), got %d", c)
	}
	if c := authProbe(pool, adminhttp.OriginPublic, true, "Bearer "+apitoken.TokenPrefix+"deadbeefdead_nope"); c != http.StatusUnauthorized {
		t.Fatalf("unknown ppk_ token on public: want 401, got %d", c)
	}
}
