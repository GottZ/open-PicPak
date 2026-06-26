package ota

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects to the test DB named by PICPAK_OPS_TEST_DSN and skips when unset
// (air-gap: the DSN/host/secret is never a code literal — it lives only in the env).
// The session is writable (default_transaction_read_only off), which the DirectPGXWriter
// rollback test needs; the Store reads ride it read-only.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PICPAK_OPS_TEST_DSN")
	if dsn == "" {
		t.Skip("PICPAK_OPS_TEST_DSN not set; skipping OTA DB tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test DB: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test DB: %v", err)
	}
	return pool
}

// TestStore_Reads proves the read path loads channels (the two seeded: stable, beta) and
// firmware_versions (0 on a fresh DB) without error.
func TestStore_Reads(t *testing.T) {
	pool := testPool(t)
	s := NewStore(pool, 15*time.Second)

	state, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Store.Load: %v", err)
	}

	byName := map[string]bool{}
	for _, c := range state.Channels {
		byName[c.Name] = true
	}
	if !byName["stable"] || !byName["beta"] {
		t.Fatalf("expected seeded channels stable+beta, got %+v", state.Channels)
	}
	t.Logf("Store read %d channel(s), %d firmware version(s), %d rollout(s)",
		len(state.Channels), len(state.Versions), len(state.Rollouts))

	// firmware_versions is allowed to be empty on a fresh DB; the read must not error.
	for _, v := range state.Versions {
		if !ValidSHA256(v.SHA256) {
			t.Errorf("firmware_versions row %q has a non-lowercase-hex sha256 %q", v.Version, v.SHA256)
		}
	}
}
