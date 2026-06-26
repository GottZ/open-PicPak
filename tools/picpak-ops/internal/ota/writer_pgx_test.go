package ota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fwCount(ctx context.Context, t *testing.T, q execer) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM firmware_versions").Scan(&n); err != nil {
		t.Fatalf("count firmware_versions: %v", err)
	}
	return n
}

// TestDirectPGXWriter_RegisterFirmware_TxRollback is the write-path safety proof: the
// exact integrity path (lowercase-sha gate + INSERT) runs inside a transaction this test
// owns and ROLLS BACK, so the row is visible WITHIN the tx (proving the CHECK passes for
// a lowercase sha) but the DB is left untouched (stays at its pre-test count, 0 on a
// fresh DB). The NEGATIVE half proves an UPPERCASE sha is rejected by the tool BEFORE the
// write (and would also violate the DB CHECK).
func TestDirectPGXWriter_RegisterFirmware_TxRollback(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	w := NewDirectPGXWriter(pool, 15*time.Second)

	before := fwCount(ctx, t, pool)

	// A unique probe version so a leak (were the rollback to fail) is unambiguous.
	probe := fmt.Sprintf("rb-%d", time.Now().UnixNano())
	if len(probe) > 31 {
		probe = probe[:31] // honor the version_len <= 31 CHECK
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// registerFirmware is the SAME code RegisterFirmware runs (it only adds begin/commit).
	if err := w.registerFirmware(ctx, tx, RegisterFirmware{
		Version:   probe,
		SHA256:    goodSHA, // lowercase: the CHECK passes
		SizeBytes: 1048576,
		Notes:     "rollback probe",
		BlobPath:  "probe/firmware.bin", // relative key, no bytes → no filesystem write
	}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("registerFirmware (lowercase sha) should succeed inside the tx: %v", err)
	}
	if got := fwCount(ctx, t, tx); got != before+1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("within tx firmware_versions = %d, want %d (the INSERT is visible pre-commit)", got, before+1)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after := fwCount(ctx, t, pool)
	if after != before {
		t.Fatalf("firmware_versions changed across the rolled-back probe: before=%d after=%d", before, after)
	}

	// NEGATIVE: an uppercase sha is rejected before any DB work (no row, no execer call
	// reaches the INSERT). Pass a tx anyway to prove the gate precedes the write.
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin(2): %v", err)
	}
	defer tx2.Rollback(ctx)
	err = w.registerFirmware(ctx, tx2, RegisterFirmware{
		Version:  "upper",
		SHA256:   strings.ToUpper(goodSHA),
		BlobPath: "x/firmware.bin",
	})
	if !errors.Is(err, ErrBadSHA) {
		t.Fatalf("uppercase sha should return ErrBadSHA, got %v", err)
	}
	if got := fwCount(ctx, t, tx2); got != before {
		t.Fatalf("uppercase sha must not insert; within tx count = %d, want %d", got, before)
	}
}

// TestDirectPGXWriter_FKAndStateMapping proves the SQLSTATE→typed-error mapping for the
// other write methods using NEGATIVE cases that the writer's own tx ROLLS BACK (no
// pollution): an unknown channel/version → ErrUnknownRef, an unknown rollout id →
// ErrUnknownRef, and a bad state → ErrBadState (pre-write gate). On a fresh DB
// (firmware_versions empty) even "set stable default to 0.6.2" is an FK violation, which
// doubles as the FK-mapping proof.
func TestDirectPGXWriter_FKAndStateMapping(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	w := NewDirectPGXWriter(pool, 15*time.Second)

	before := fwCount(ctx, t, pool)

	// Unknown channel → 0 rows affected → ErrUnknownRef (rolled back, no change).
	if err := w.SetChannelDefault(ctx, SetChannelDefault{Channel: "no-such-channel", Version: "0.6.2"}); !errors.Is(err, ErrUnknownRef) {
		t.Errorf("SetChannelDefault(unknown channel) → %v, want ErrUnknownRef", err)
	}

	// Known channel, unknown version → FK violation (23503) → ErrUnknownRef.
	if err := w.SetChannelDefault(ctx, SetChannelDefault{Channel: "stable", Version: "definitely-not-registered"}); !errors.Is(err, ErrUnknownRef) {
		t.Errorf("SetChannelDefault(unknown version) → %v, want ErrUnknownRef", err)
	}

	// Pin to an unknown version → FK violation → ErrUnknownRef.
	if err := w.PinRollout(ctx, PinRollout{Serial: "*", Channel: "stable", Version: "definitely-not-registered", Pinned: true}); !errors.Is(err, ErrUnknownRef) {
		t.Errorf("PinRollout(unknown version) → %v, want ErrUnknownRef", err)
	}

	// Unknown rollout id → 0 rows → ErrUnknownRef.
	if err := w.SetRolloutState(ctx, SetRolloutState{ID: -1, State: StatePaused}); !errors.Is(err, ErrUnknownRef) {
		t.Errorf("SetRolloutState(unknown id) → %v, want ErrUnknownRef", err)
	}

	// Bad state → pre-write gate → ErrBadState.
	if err := w.SetRolloutState(ctx, SetRolloutState{ID: 1, State: "bogus"}); !errors.Is(err, ErrBadState) {
		t.Errorf("SetRolloutState(bad state) → %v, want ErrBadState", err)
	}

	if after := fwCount(ctx, t, pool); after != before {
		t.Fatalf("DB polluted by negative-mapping tests: before=%d after=%d", before, after)
	}
}
