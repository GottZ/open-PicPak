package adminsession

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (ephemeral postgres, migrations
// 0001..0011 + 0014). A session needs an admin_users row (FK), seeded here.
func dbPool(t *testing.T) (*pgxpool.Pool, int64) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE admin_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	var uid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO admin_users (username, password_hash) VALUES ('sess-user', 'x') RETURNING id`).
		Scan(&uid); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return pool, uid
}

// TestCreateResolveDelete: a fresh session resolves to its user; the stored id is the sha256 of the
// secret (hash-at-rest, not the secret itself); delete makes it unresolvable.
func TestCreateResolveDelete(t *testing.T) {
	pool, uid := dbPool(t)
	ctx := context.Background()

	secret, sess, err := Create(ctx, pool, uid, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.UserID != uid {
		t.Fatalf("session user: %d != %d", sess.UserID, uid)
	}

	// The row id must be sha256(secret), never the plaintext secret.
	var idHits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions WHERE id = $1::bytea`, []byte(secret)).Scan(&idHits); err != nil {
		t.Fatalf("scan id hits: %v", err)
	}
	if idHits != 0 {
		t.Fatal("session id stored the plaintext secret")
	}

	got, ok, err := Resolve(ctx, pool, secret)
	if err != nil || !ok || got.UserID != uid {
		t.Fatalf("resolve: ok=%v user=%d err=%v", ok, got.UserID, err)
	}

	if err := Delete(ctx, pool, secret); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, err := Resolve(ctx, pool, secret); ok || err != nil {
		t.Fatalf("resolve after delete: want (false,nil), got (%v,%v)", ok, err)
	}
}

// TestExpiryAndCleanup: an expired session does not resolve and is purged by Cleanup.
func TestExpiryAndCleanup(t *testing.T) {
	pool, uid := dbPool(t)
	ctx := context.Background()

	secret, _, err := Create(ctx, pool, uid, -time.Minute) // already expired
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok, err := Resolve(ctx, pool, secret); ok || err != nil {
		t.Fatalf("resolve expired: want (false,nil), got (%v,%v)", ok, err)
	}
	n, err := Cleanup(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("Cleanup: n=%d err=%v (want 1)", n, err)
	}
}
