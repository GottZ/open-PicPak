package devicestore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/rollout"
)

// T3b: the registration serial gate. Red: a looser/no check admits a 40-char or quote/space serial
// that the on-device sn[32] truncates → the pubkey is keyed to a serial the device never presents.
func TestValidSerial(t *testing.T) {
	good := []string{"A", "0", "abc-123_XYZ", "AB12CD3", strings.Repeat("a", 31)}
	bad := []string{"", "*", strings.Repeat("a", 32), "has space", `q"uote`, "a/b", "a.b", "a\tb", "ünïcode"}
	for _, s := range good {
		if !ValidSerial(s) {
			t.Errorf("ValidSerial(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSerial(s) {
			t.Errorf("ValidSerial(%q) = true, want false", s)
		}
	}
}

// The pubkey gate accepts only a real on-curve 65-byte P-256 point — not a length-only blob.
func TestValidPubkey(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := elliptic.Marshal(elliptic.P256(), priv.X, priv.Y) //nolint:staticcheck // raw point for the on-wire gate
	if len(pub) != 65 || !ValidPubkey(pub) {
		t.Fatal("a real P-256 point was rejected")
	}
	offCurve := make([]byte, 65)
	offCurve[0] = 0x04 // 0x04 || 64 zero bytes — correct framing, off the curve
	if ValidPubkey(offCurve) {
		t.Fatal("off-curve point accepted")
	}
	if ValidPubkey(pub[:64]) {
		t.Fatal("64-byte (short) blob accepted")
	}
	bad := append([]byte{0x05}, pub[1:]...) // wrong leading byte (compressed/invalid)
	if ValidPubkey(bad) {
		t.Fatal("non-0x04 prefix accepted")
	}
	if ValidPubkey(nil) {
		t.Fatal("nil accepted")
	}
}

// --- DB property tests (skipped unless TEST_DATABASE_URL is set; run in the e2e gate) ---

const shaHex = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 64 hex

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
	for _, stmt := range []string{
		`TRUNCATE rollout_targets`,
		`UPDATE channels SET default_version = NULL`,
		`DELETE FROM devices`,
		`DELETE FROM firmware_versions`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
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

// T14 (A20 D20.10, the OTA half of the device-delete tx) — deleting a device removes its per-serial
// rollout_targets orphan IN THE SAME TX (no FK is possible: '*' precludes one), while the channel '*'
// fleet row survives. The fall-through is proven concretely: a RE-registered serial then resolves to
// the fleet version, NOT the stale per-serial pin it once had. Red: no cascade → the orphan silently
// re-pins a re-created serial to a stale version.
func TestDelete_CascadesRolloutTargets_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes)
	                   VALUES ('v1',$1,'v1.bin',1), ('v2',$1,'v2.bin',1)`, shaHex)
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)
	mustExec(t, pool, `INSERT INTO rollout_targets (serial, channel, version, state)
	                   VALUES ('dev1','stable','v1','active'), ('*','stable','v2','active')`)

	found, err := Delete(ctx, pool, "dev1")
	if err != nil || !found {
		t.Fatalf("delete dev1: found=%v err=%v", found, err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rollout_targets WHERE serial='dev1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("orphan per-serial rollout survived the delete (%d rows)", n)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rollout_targets WHERE serial='*'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the '*' fleet rollout was wrongly deleted (%d rows, want 1)", n)
	}

	// fall-through: a re-registered serial resolves to the FLEET version, not the stale v1 pin.
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)
	res, err := rollout.ResolveTarget(ctx, pool, "dev1")
	if err != nil || res.Version != "v2" || res.Source != rollout.SourceFleet {
		t.Errorf("re-registered resolve = %+v (err %v), want v2/fleet", res, err)
	}
}
