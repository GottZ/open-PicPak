package adminuser

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (ephemeral postgres, migrations
// 0001..0011 + 0014). Mirrors imgstore/store_test.go.
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
	// CASCADE also clears admin_sessions (FK to admin_users).
	if _, err := pool.Exec(context.Background(), `TRUNCATE admin_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// TestVerify is the admin_users negative probe (c): a correct password verifies; a wrong password
// does not; and a DISABLED account fails verify even with the correct password. Also covers the
// unknown-username path (false, no error, dummy-verify keeps latency uniform).
func TestVerify(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	u, err := Create(ctx, pool, "alice", "s3cret-pw", true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if u.Username != "alice" || !u.IsAdmin {
		t.Fatalf("created user: %+v", u)
	}

	// Correct password.
	got, ok, err := Verify(ctx, pool, "alice", "s3cret-pw")
	if err != nil || !ok || got.ID != u.ID {
		t.Fatalf("verify correct: ok=%v id=%d err=%v", ok, got.ID, err)
	}

	// Wrong password.
	if _, ok, err := Verify(ctx, pool, "alice", "wrong"); ok || err != nil {
		t.Fatalf("verify wrong: want (false,nil), got (%v,%v)", ok, err)
	}

	// Unknown username.
	if _, ok, err := Verify(ctx, pool, "nobody", "s3cret-pw"); ok || err != nil {
		t.Fatalf("verify unknown: want (false,nil), got (%v,%v)", ok, err)
	}

	// Disabled account fails even with the correct password.
	if err := SetDisabled(ctx, pool, u.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if _, ok, err := Verify(ctx, pool, "alice", "s3cret-pw"); ok || err != nil {
		t.Fatalf("verify disabled+correct: want (false,nil), got (%v,%v)", ok, err)
	}

	// Re-enabling restores access.
	if err := SetDisabled(ctx, pool, u.ID, false); err != nil {
		t.Fatalf("SetDisabled re-enable: %v", err)
	}
	if _, ok, err := Verify(ctx, pool, "alice", "s3cret-pw"); !ok || err != nil {
		t.Fatalf("verify re-enabled: want (true,nil), got (%v,%v)", ok, err)
	}
}

// TestCreateDuplicateUsername: a duplicate username is ErrUsernameTaken.
func TestCreateDuplicateUsername(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	if _, err := Create(ctx, pool, "bob", "pw1", false); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := Create(ctx, pool, "bob", "pw2", false); err != ErrUsernameTaken {
		t.Fatalf("duplicate: want ErrUsernameTaken, got %v", err)
	}
}
