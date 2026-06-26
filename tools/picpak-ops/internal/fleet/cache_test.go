package fleet_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/db"
	"github.com/open-picpak/picpak-ops/internal/fleet"
)

// testDSNEnv carries the test DSN; never a literal here. Unset → the DB test
// skips, so the package builds/tests clean with no database (air-gap).
const testDSNEnv = "PICPAK_OPS_TEST_DSN"

func testConfig(dsn string) *config.Config {
	return &config.Config{
		Database: config.Database{
			DSN:              dsn,
			MaxConns:         4,
			ConnectTimeout:   config.Duration(5 * time.Second),
			StatementTimeout: config.Duration(15 * time.Second),
			ReadOnly:         true,
		},
	}
}

// TestCache_LoadReadsDevices loads the real devices rows over the shared read-only
// pool and checks the accessors: List/BySerial agree, an unknown serial misses,
// and the count is consistent. It asserts count >= 0 (an empty fleet is valid) and
// that BySerial round-trips for any loaded serial.
func TestCache_LoadReadsDevices(t *testing.T) {
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping DB-backed fleet test (no database needed to build)", testDSNEnv)
	}
	ctx := context.Background()

	pool, err := db.NewPool(ctx, testConfig(dsn))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	c := fleet.New(pool)
	if err := c.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}

	list := c.List()
	if len(list) != c.Len() {
		t.Fatalf("List length %d != Len %d", len(list), c.Len())
	}
	t.Logf("fleet cache loaded %d device(s)", len(list))

	// List must be sorted by serial (stable picker order).
	for i := 1; i < len(list); i++ {
		if list[i-1].Serial > list[i].Serial {
			t.Fatalf("List not sorted by serial at %d: %q > %q", i, list[i-1].Serial, list[i].Serial)
		}
	}

	// BySerial round-trips for every loaded serial.
	for _, d := range list {
		got, ok := c.BySerial(d.Serial)
		if !ok {
			t.Fatalf("BySerial(%q) missing though present in List", d.Serial)
		}
		if got.Serial != d.Serial || got.Channel != d.Channel {
			t.Fatalf("BySerial(%q) mismatch: got %+v want %+v", d.Serial, got, d)
		}
	}

	// An unknown serial misses cleanly.
	if _, ok := c.BySerial("PP-DEFINITELY-NOT-A-REAL-SERIAL"); ok {
		t.Fatal("BySerial returned ok for an unknown serial")
	}
}
