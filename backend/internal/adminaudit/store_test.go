package adminaudit

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property test — skipped unless TEST_DATABASE_URL is set (ephemeral postgres, migrations
// 0001..0011 + 0014).
func dbPool(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(context.Background(), `TRUNCATE admin_audit RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// TestWrite: a written entry round-trips; empty optional fields land as NULL.
func TestWrite(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	if err := Write(ctx, pool, Entry{
		ActorKind: "bearer", ActorID: "abc123", Action: "token.mint",
		Target: "def456", RemoteIP: "203.0.113.7", OK: true,
	}); err != nil {
		t.Fatalf("Write full: %v", err)
	}
	// Empty ActorID/Target/RemoteIP -> NULL.
	if err := Write(ctx, pool, Entry{ActorKind: "cli", Action: "session.login_fail", OK: false}); err != nil {
		t.Fatalf("Write sparse: %v", err)
	}

	var total, nulls int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit WHERE actor_id IS NULL AND target IS NULL AND remote_ip IS NULL`).
		Scan(&nulls); err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if total != 2 || nulls != 1 {
		t.Fatalf("rows: total=%d nulls=%d (want 2, 1)", total, nulls)
	}
}
