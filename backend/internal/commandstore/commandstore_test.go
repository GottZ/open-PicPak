package commandstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Script size is validated before any DB access (a nil pool proves it): empty and over-cap are
// rejected up front. Red: an unchecked script reaches the queue (an empty no-op, or a RAM-spiking blob).
// The cap is OCTET-based (K3): a multibyte script whose byte length exceeds 8191 must be rejected even
// when its codepoint count would have passed the old char_length(<=16384) CHECK.
func TestEnqueue_ScriptValidation(t *testing.T) {
	if _, err := Enqueue(context.Background(), nil, "*", "", nil, 1, nil); !errors.Is(err, ErrScriptEmpty) {
		t.Fatalf("empty: want ErrScriptEmpty, got %v", err)
	}
	// 8192 bytes = one past the 8191 cap -> the FW buffer overflow the cap fail-closes (K3).
	if _, err := Enqueue(context.Background(), nil, "*", strings.Repeat("x", ScriptMax+1), nil, 1, nil); !errors.Is(err, ErrScriptTooLong) {
		t.Fatalf("over-cap 8192: want ErrScriptTooLong, got %v", err)
	}
	// 8193 bytes (the masterplan gate) -> rejected.
	if _, err := Enqueue(context.Background(), nil, "*", strings.Repeat("x", 8193), nil, 1, nil); !errors.Is(err, ErrScriptTooLong) {
		t.Fatalf("over-cap 8193: want ErrScriptTooLong, got %v", err)
	}
	// Multibyte probe: 8191 codepoints of a 2-byte rune = 16382 octets -> len() (octets) > 8191 -> rejected,
	// though char_length (8191 codepoints) would have passed the old cap.
	mb := strings.Repeat("ö", 8191) // ö = 0xC3 0xB6 (2 bytes)
	if len(mb) != 16382 {
		t.Fatalf("multibyte fixture: want 16382 octets, got %d", len(mb))
	}
	if _, err := Enqueue(context.Background(), nil, "*", mb, nil, 1, nil); !errors.Is(err, ErrScriptTooLong) {
		t.Fatalf("multibyte over-cap: want ErrScriptTooLong, got %v", err)
	}
}

// DB property tests — skipped unless TEST_DATABASE_URL is set (run in the e2e gate against an ephemeral
// postgres with migrations 0001..0017 applied). Mirrors plrender/faasstore.
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
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE command_queue, device_c2_cursor, devices, operator_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// opKey seeds one operator_key (the enqueue FK target) and returns its id — the forensic stamp every
// enqueue carries (SEC-M2 / D17.9). token_hash is a fixed 32-byte blob (the CHECK requires octet_length 32).
func opKey(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO operator_keys (token_hash, label) VALUES ($1,'test') RETURNING id`,
		make([]byte, 32)).Scan(&id); err != nil {
		t.Fatalf("seed operator_key: %v", err)
	}
	return id
}

func rowsFor(t *testing.T, pool *pgxpool.Pool, serial string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM command_queue WHERE serial = $1`, serial).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", serial, err)
	}
	return n
}

// TestEnqueue_BoundaryAccepted proves the 8191-byte boundary is accepted (the exact FW-accepted maximum),
// the complement to the over-cap rejection above.
func TestEnqueue_BoundaryAccepted(t *testing.T) {
	pool := dbPool(t)
	op := opKey(t, pool)
	seq, err := Enqueue(context.Background(), pool, "*", strings.Repeat("x", ScriptMax), nil, op, nil)
	if err != nil || seq <= 0 {
		t.Fatalf("8191-byte script: want accepted, got seq=%d err=%v", seq, err)
	}
}

// TestEnqueue_FleetDedup proves the K4 guard: a double-clicked identical '*' apply dedups to ONE row
// (idempotent — the existing seq comes back, no error), while a different key stays parallel-enqueuable.
func TestEnqueue_FleetDedup(t *testing.T) {
	pool := dbPool(t)
	op := opKey(t, pool)
	ctx := context.Background()
	key := "tmpl:7:abc123"

	seq1, err := Enqueue(ctx, pool, "*", "reboot()", nil, op, &key)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Identical still-pending command -> no second row, same seq returned (red: 2 rows without the guard).
	seq2, err := Enqueue(ctx, pool, "*", "reboot()", nil, op, &key)
	if err != nil {
		t.Fatalf("dup enqueue: %v", err)
	}
	if seq2 != seq1 {
		t.Fatalf("dup enqueue: want existing seq %d, got %d", seq1, seq2)
	}
	if n := rowsFor(t, pool, "*"); n != 1 {
		t.Fatalf("after dup: want 1 row, got %d", n)
	}

	// A different key is a different command -> stays parallel-enqueuable (regression: non-identical
	// commands to the same target are not collapsed).
	key2 := "tmpl:7:def456"
	if _, err := Enqueue(ctx, pool, "*", "net_clear()", nil, op, &key2); err != nil {
		t.Fatalf("second key enqueue: %v", err)
	}
	if n := rowsFor(t, pool, "*"); n != 2 {
		t.Fatalf("after second key: want 2 rows, got %d", n)
	}

	// Keyless enqueues stay append-only (legacy path, no dedup).
	if _, err := Enqueue(ctx, pool, "*", "reboot()", nil, op, nil); err != nil {
		t.Fatalf("keyless a: %v", err)
	}
	if _, err := Enqueue(ctx, pool, "*", "reboot()", nil, op, nil); err != nil {
		t.Fatalf("keyless b: %v", err)
	}
	if n := rowsFor(t, pool, "*"); n != 4 {
		t.Fatalf("after keyless pair: want 4 rows, got %d", n)
	}
}

// TestEnqueue_ReentryAfterAck proves the partial-index semantics: once a targeted device acks the command
// (MarkApplied on the serve cursor advance), an identical command may be enqueued again — the applied row
// has left the pending-dedup index. It also covers the non-identical-parallel regression on a serial.
func TestEnqueue_ReentryAfterAck(t *testing.T) {
	pool := dbPool(t)
	op := opKey(t, pool)
	ctx := context.Background()
	const serial = "PP-DEDUP-1"
	if _, err := pool.Exec(ctx, `INSERT INTO devices (serial, channel) VALUES ($1,'stable')`, serial); err != nil {
		t.Fatalf("device fixture: %v", err)
	}
	key := "tmpl:7:abc123"

	seq1, err := Enqueue(ctx, pool, serial, "reboot()", nil, op, &key)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Still pending -> dedup to the same row.
	if seq2, err := Enqueue(ctx, pool, serial, "reboot()", nil, op, &key); err != nil || seq2 != seq1 {
		t.Fatalf("dup while pending: want seq %d, got %d err=%v", seq1, seq2, err)
	}
	if n := rowsFor(t, pool, serial); n != 1 {
		t.Fatalf("while pending: want 1 row, got %d", n)
	}

	// The device acks up to seq1 (production serve path calls MarkApplied inside the cursor-advance tx).
	if err := MarkApplied(ctx, pool, serial, seq1); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	// Now an identical command re-enters (a new row, new seq) — the applied row left the dedup index.
	seq3, err := Enqueue(ctx, pool, serial, "reboot()", nil, op, &key)
	if err != nil {
		t.Fatalf("re-enqueue after ack: %v", err)
	}
	if seq3 == seq1 {
		t.Fatalf("re-enqueue after ack: want a new seq, got the applied one %d", seq1)
	}
	if n := rowsFor(t, pool, serial); n != 2 {
		t.Fatalf("after re-entry: want 2 rows, got %d", n)
	}

	// Regression: a non-identical command to the same device is always parallel-enqueuable.
	other := "tmpl:9:zzz"
	if _, err := Enqueue(ctx, pool, serial, "net_clear()", nil, op, &other); err != nil {
		t.Fatalf("non-identical enqueue: %v", err)
	}
	if n := rowsFor(t, pool, serial); n != 3 {
		t.Fatalf("after non-identical: want 3 rows, got %d", n)
	}
}
