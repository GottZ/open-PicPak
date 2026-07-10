package commandstore

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pushPool opens the ephemeral DB and truncates the tables the direct-display bridge touches (the base
// dbPool does not clear playlist/binding rows). Skipped unless TEST_DATABASE_URL is set.
func pushPool(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE command_queue, device_playlist_binding, playlist, devices, operator_keys
		 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestEnqueuePlaylistRefresh_BatchAndDedup is the direct-display gate. Positive: one call fans the
// fixed refresh out to EVERY bound + registered device in a single statement. Negative (K4, red
// without the idempotency key): a double-trigger must NOT double the pending commands — each serial
// keeps exactly ONE pending refresh. Also: an unbound device gets nothing, and the script is the
// fixed refresh() intent (no operator/playlist-derived input, §5).
func TestEnqueuePlaylistRefresh_BatchAndDedup(t *testing.T) {
	pool := pushPool(t)
	ctx := context.Background()
	op := opKey(t, pool)

	var plID int64
	if err := pool.QueryRow(ctx, `INSERT INTO playlist (name) VALUES ('pl') RETURNING id`).Scan(&plID); err != nil {
		t.Fatalf("seed playlist: %v", err)
	}
	bound := []string{"AA", "BB", "CC"}
	for _, sn := range bound {
		mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ($1,'stable')`, sn)
		mustExec(t, pool, `INSERT INTO device_playlist_binding (serial, playlist_id) VALUES ($1,$2)`, sn, plID)
	}
	// An unbound registered device — must never receive a refresh (§6 fan-out is over bindings only).
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('ZZ','stable')`)

	// First fan-out: one row per bound serial, in ONE statement.
	n, err := EnqueuePlaylistRefresh(ctx, pool, plID, &op)
	if err != nil {
		t.Fatalf("first fan-out: %v", err)
	}
	if n != len(bound) {
		t.Fatalf("first fan-out enqueued %d, want %d (one refresh per bound serial)", n, len(bound))
	}

	// Double-trigger (double-clicked edit): the K4 partial-index guard dedups → ZERO new rows, and
	// every serial still holds exactly ONE pending refresh (red without the key: n2=3, 2 rows/serial).
	n2, err := EnqueuePlaylistRefresh(ctx, pool, plID, &op)
	if err != nil {
		t.Fatalf("double-trigger: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("double-trigger enqueued %d new rows, want 0 (K4 dedup)", n2)
	}
	for _, sn := range bound {
		if got := rowsFor(t, pool, sn); got != 1 {
			t.Fatalf("serial %s holds %d pending refreshes, want exactly 1 (K4)", sn, got)
		}
	}
	if got := rowsFor(t, pool, "ZZ"); got != 0 {
		t.Fatalf("unbound device ZZ received %d refreshes, want 0", got)
	}

	// The enqueued command is the fixed refresh() intent, operator-attributed.
	var script string
	var gotOp *int64
	if err := pool.QueryRow(ctx,
		`SELECT script, operator_key_id FROM command_queue WHERE serial = 'AA'`).Scan(&script, &gotOp); err != nil {
		t.Fatalf("read enqueued command: %v", err)
	}
	if script != PlaylistRefreshScript {
		t.Fatalf("enqueued script %q, want %q (fixed server-authored refresh)", script, PlaylistRefreshScript)
	}
	if gotOp == nil || *gotOp != op {
		t.Fatalf("operator attribution = %v, want %d", gotOp, op)
	}
}

// TestEnqueuePlaylistRefresh_NullOperator proves the server-triggered path (warmer fan-out) enqueues
// with a NULL attribution — the command_queue FK column is nullable, so a mutation side-effect refresh
// need not fabricate an operator.
func TestEnqueuePlaylistRefresh_NullOperator(t *testing.T) {
	pool := pushPool(t)
	ctx := context.Background()

	var plID int64
	if err := pool.QueryRow(ctx, `INSERT INTO playlist (name) VALUES ('pl') RETURNING id`).Scan(&plID); err != nil {
		t.Fatalf("seed playlist: %v", err)
	}
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('AA','stable')`)
	mustExec(t, pool, `INSERT INTO device_playlist_binding (serial, playlist_id) VALUES ('AA',$1)`, plID)

	n, err := EnqueuePlaylistRefresh(ctx, pool, plID, nil)
	if err != nil || n != 1 {
		t.Fatalf("null-operator fan-out: n=%d err=%v, want n=1", n, err)
	}
	var opNull *int64
	if err := pool.QueryRow(ctx,
		`SELECT operator_key_id FROM command_queue WHERE serial = 'AA'`).Scan(&opNull); err != nil {
		t.Fatalf("read attribution: %v", err)
	}
	if opNull != nil {
		t.Fatalf("operator_key_id = %d, want NULL (server-side side-effect)", *opNull)
	}
}
