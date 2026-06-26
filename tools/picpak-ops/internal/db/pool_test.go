package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// testDSNEnv is the env var carrying the test DSN. It is NEVER a literal in this
// file: the password lives in the gitignored backend/.env and is injected only for
// the gate run. When unset the DB tests skip, so the package builds/tests clean
// with no database (air-gap: no secret in code).
const testDSNEnv = "PICPAK_OPS_TEST_DSN"

// testConfig builds a minimal *config.Config carrying only the [database] fields
// NewPool reads. It does not go through config.Load (no file/validation needed for
// the pool); the DSN comes only from the environment.
func testConfig(dsn string, readOnly bool) *config.Config {
	return &config.Config{
		Database: config.Database{
			DSN:              dsn,
			MaxConns:         4,
			ConnectTimeout:   config.Duration(5 * time.Second),
			StatementTimeout: config.Duration(15 * time.Second),
			ReadOnly:         readOnly,
		},
	}
}

// requireDSN returns the test DSN or skips the test when it is unset.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping DB-backed test (no database needed to build)", testDSNEnv)
	}
	return dsn
}

// TestPool_ConnectsAndPings proves NewPool reaches the backend and the standalone
// Ping helper round-trips, under the default read_only=true.
func TestPool_ConnectsAndPings(t *testing.T) {
	dsn := requireDSN(t)
	ctx := context.Background()

	pool, err := NewPool(ctx, testConfig(dsn, true))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := Ping(pingCtx, pool); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestPool_ReadOnlyBlocksWrites is the NEGATIVE safety proof: under
// read_only=true, both a DML write into a real table (devices) and a DDL write
// (CREATE TEMP TABLE) are rejected by the server with SQLSTATE 25006
// (read_only_sql_transaction). This is the analog of the flash negative gate —
// the read-only enforcement is proven by observing the write actually blocked.
func TestPool_ReadOnlyBlocksWrites(t *testing.T) {
	dsn := requireDSN(t)
	ctx := context.Background()

	pool, err := NewPool(ctx, testConfig(dsn, true))
	if err != nil {
		t.Fatalf("NewPool(read_only=true): %v", err)
	}
	defer pool.Close()

	// A unique probe serial so a real INSERT (were it not blocked) could not
	// collide with an existing row and mask the read-only error as a PK conflict.
	probe := fmt.Sprintf("PP-OPS-RO-PROBE-%d", time.Now().UnixNano())

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"insert-real-table", "INSERT INTO devices (serial, channel) VALUES ($1, 'stable')", []any{probe}},
		{"create-temp-table", "CREATE TEMP TABLE picpak_ops_ro_probe (x int)", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("write %q succeeded under read_only=true; the read-only session did NOT block it", tc.name)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("write %q blocked, but not by a server error (got %T: %v)", tc.name, err, err)
			}
			if pgErr.Code != sqlstateReadOnly {
				t.Fatalf("write %q blocked with SQLSTATE %s (%s); expected %s (read-only)", tc.name, pgErr.Code, pgErr.Message, sqlstateReadOnly)
			}
			t.Logf("blocked %q: SQLSTATE %s — %s", tc.name, pgErr.Code, pgErr.Message)
		})
	}
}

// TestPool_ReadWriteAllowsWrites is the positive half of the safety proof: with
// read_only=false the SAME write succeeds — performed inside a transaction that is
// ROLLED BACK so the real devices table is untouched. Together with the negative
// test this shows the block is the read-only enforcement, not a permission or
// connectivity artifact.
func TestPool_ReadWriteAllowsWrites(t *testing.T) {
	dsn := requireDSN(t)
	ctx := context.Background()

	pool, err := NewPool(ctx, testConfig(dsn, false))
	if err != nil {
		t.Fatalf("NewPool(read_only=false): %v", err)
	}
	defer pool.Close()

	before := deviceCount(ctx, t, pool)

	probe := fmt.Sprintf("PP-OPS-RW-PROBE-%d", time.Now().UnixNano())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO devices (serial, channel) VALUES ($1, 'stable')", probe); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("INSERT under read_only=false should succeed, got: %v", err)
	}
	// ROLLBACK: the write was accepted by the server (proving writes are allowed)
	// but is discarded, leaving the real data untouched.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after := deviceCount(ctx, t, pool)
	if before != after {
		t.Fatalf("devices row count changed across the rolled-back probe write: before=%d after=%d", before, after)
	}
	if _, ok := lookupSerial(ctx, t, pool, probe); ok {
		t.Fatalf("probe serial %q leaked into devices despite rollback", probe)
	}
}

func deviceCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM devices").Scan(&n); err != nil {
		t.Fatalf("count devices: %v", err)
	}
	return n
}

func lookupSerial(ctx context.Context, t *testing.T, pool *pgxpool.Pool, serial string) (string, bool) {
	t.Helper()
	var got string
	if err := pool.QueryRow(ctx, "SELECT serial FROM devices WHERE serial = $1", serial).Scan(&got); err != nil {
		return "", false
	}
	return got, true
}
