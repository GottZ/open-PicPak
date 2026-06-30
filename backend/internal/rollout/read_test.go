package rollout

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// --- DB property tests (skipped unless TEST_DATABASE_URL is set; run in the e2e gate) ---.

// shaHex is a valid 64-lowercase-hex placeholder satisfying the firmware_versions.sha256 CHECK
// (0001:22). Resolution never reads the sha, so a constant is fine here.
const shaHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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
	ctx := context.Background()
	// FK-safe reset: clear the version references (rollout rows + channel defaults) BEFORE deleting
	// firmware_versions, and DELETE devices (cascades device_auth/cursor/fragment) rather than TRUNCATE
	// firmware_versions CASCADE — which would also wipe the seeded 'stable'/'beta' channels (channels
	// FK-references firmware_versions).
	for _, stmt := range []string{
		`TRUNCATE rollout_targets`,
		`UPDATE channels SET default_version = NULL`,
		`DELETE FROM devices`,
		`DELETE FROM firmware_versions`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func seedFW(t *testing.T, pool *pgxpool.Pool, versions ...string) {
	t.Helper()
	for _, v := range versions {
		mustExec(t, pool,
			`INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ($1,$2,$3,1)`,
			v, shaHex, v+".bin")
	}
}

func assertResolve(t *testing.T, pool *pgxpool.Pool, serial string, want Resolved) {
	t.Helper()
	got, err := ResolveTarget(context.Background(), pool, serial)
	if err != nil {
		t.Fatalf("ResolveTarget(%q): %v", serial, err)
	}
	if got != want {
		t.Errorf("ResolveTarget(%q) = %+v, want %+v", serial, got, want)
	}
}

// TestResolveTarget_DB walks the full precedence ladder through the live schema, mutating one tier at a
// time: default → fleet → per-serial → (pause per-serial) → fleet again. Each step is the same red as
// the pure T4 but proves read.go scopes the rows and channel correctly.
func TestResolveTarget_DB(t *testing.T) {
	pool := dbPool(t)
	seedFW(t, pool, "v-sn", "v-fleet", "v-def")
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)

	// 1. channel default only
	mustExec(t, pool, `UPDATE channels SET default_version='v-def' WHERE name='stable'`)
	assertResolve(t, pool, "dev1", Resolved{Version: "v-def", Source: SourceChannelDefault})

	// 2. fleet active beats default
	mustExec(t, pool, `INSERT INTO rollout_targets (serial, channel, version, state) VALUES ('*','stable','v-fleet','active')`)
	assertResolve(t, pool, "dev1", Resolved{Version: "v-fleet", Source: SourceFleet})

	// 3. per-serial active beats fleet (D20.3)
	mustExec(t, pool, `INSERT INTO rollout_targets (serial, channel, version, state) VALUES ('dev1','stable','v-sn','active')`)
	assertResolve(t, pool, "dev1", Resolved{Version: "v-sn", Source: SourceSerial})

	// 4. pause the per-serial pin → it offers nothing → falls back to the fleet (D20.3 skip)
	mustExec(t, pool, `UPDATE rollout_targets SET state='paused' WHERE serial='dev1' AND channel='stable'`)
	assertResolve(t, pool, "dev1", Resolved{Version: "v-fleet", Source: SourceFleet})

	// 5. unknown serial → none, no error (fail-open input to D20.8)
	assertResolve(t, pool, "ghost", Resolved{Source: SourceNone})
}

// TestResolveTarget_ServerChannelAuthoritative_DB proves D20.5 at the resolver: routing is by the
// server-owned devices.channel, never a device-reported channel. A device on 'stable' must NOT pull a
// beta-only fleet rollout — ResolveTarget does not even accept a reported channel, so self-promotion
// is structurally impossible here (the spoofable-header path T10 is gated again at ingest, W3).
func TestResolveTarget_ServerChannelAuthoritative_DB(t *testing.T) {
	pool := dbPool(t)
	seedFW(t, pool, "v-beta")
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)
	// A beta fleet rollout exists; dev1 is on stable and stable has no default → nothing to serve.
	mustExec(t, pool, `INSERT INTO rollout_targets (serial, channel, version, state) VALUES ('*','beta','v-beta','active')`)
	assertResolve(t, pool, "dev1", Resolved{Source: SourceNone})
}
