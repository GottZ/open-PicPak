package telemetry

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testRepo connects to the test DB named by PICPAK_OPS_TEST_DSN and skips when unset
// (air-gap: the DSN/host/secret is never a code literal — it lives only in the env).
func testRepo(t *testing.T) *Repo {
	t.Helper()
	dsn := os.Getenv("PICPAK_OPS_TEST_DSN")
	if dsn == "" {
		t.Skip("PICPAK_OPS_TEST_DSN not set; skipping telemetry DB tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test DB: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test DB: %v", err)
	}
	return NewRepo(pool, 15*time.Second)
}

func TestRepo_LatestFleet(t *testing.T) {
	repo := testRepo(t)
	rows, err := repo.LatestFleet(context.Background(), 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	if len(rows) < 1 {
		t.Fatal("expected at least one fleet row from the seeded test DB")
	}
	t.Logf("LatestFleet read %d device row(s)", len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Serial == "" {
			t.Error("fleet row has an empty serial")
		}
		if r.Time.IsZero() {
			t.Error("fleet row has a zero time")
		}
		if seen[r.Serial] {
			t.Errorf("DISTINCT ON should yield one row per serial; %q duplicated", r.Serial)
		}
		seen[r.Serial] = true
		h, _ := Verdict(r.Sample(), nil, testVerdictCfg(), time.Now())
		t.Logf("  device verdict=%s batt=%s reset=%s", h, BattString(r.BattMV, "V"), ResetReasonString(r.ResetReason))
	}
}

func TestRepo_LatestDevice(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	fleet, err := repo.LatestFleet(ctx, 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	if len(fleet) == 0 {
		t.Skip("no telemetry rows to detail")
	}
	serial := fleet[0].Serial // runtime value; never a code literal (air-gap)

	d, err := repo.LatestDevice(ctx, serial)
	if err != nil {
		t.Fatalf("LatestDevice: %v", err)
	}
	if d == nil {
		t.Fatal("expected a detail row for a known serial")
	}
	if d.Serial != serial {
		t.Fatalf("LatestDevice serial = %q, want %q", d.Serial, serial)
	}
	if d.Time.IsZero() {
		t.Fatal("detail row has a zero time")
	}
	// The extra-JSONB decode and verdict must run without panic on real data.
	_ = d.Extra
	_, _ = Verdict(d.Sample(), nil, testVerdictCfg(), time.Now())

	// An unknown serial → (nil, nil), not an error (sparse-table tolerance).
	none, err := repo.LatestDevice(ctx, "no-such-serial-xyz")
	if err != nil {
		t.Fatalf("missing serial should not error: %v", err)
	}
	if none != nil {
		t.Fatal("missing serial should yield a nil detail")
	}
}

func TestRepo_DeviceHistory(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	fleet, err := repo.LatestFleet(ctx, 1000)
	if err != nil {
		t.Fatalf("LatestFleet: %v", err)
	}
	if len(fleet) == 0 {
		t.Skip("no telemetry rows to detail")
	}
	serial := fleet[0].Serial

	hist, err := repo.DeviceHistory(ctx, serial, 72*time.Hour, 500)
	if err != nil {
		t.Fatalf("DeviceHistory: %v", err)
	}
	if len(hist) < 1 {
		t.Fatal("expected at least one history point within the window")
	}
	t.Logf("DeviceHistory read %d point(s) for the selected device", len(hist))
	// Newest-first ordering (the telemetry_serial_time (serial, time DESC) access path).
	for i := 1; i < len(hist); i++ {
		if hist[i].Time.After(hist[i-1].Time) {
			t.Fatal("history must be ordered time DESC")
		}
	}
}

// TestRepo_ConcurrentReads exercises the shared pool from several goroutines so the
// race detector has concurrency to inspect (the pool is goroutine-safe; the repo holds
// no mutable shared state).
func TestRepo_ConcurrentReads(t *testing.T) {
	repo := testRepo(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repo.LatestFleet(context.Background(), 1000); err != nil {
				t.Errorf("concurrent LatestFleet: %v", err)
			}
		}()
	}
	wg.Wait()
}
