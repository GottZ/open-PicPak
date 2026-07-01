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
		`TRUNCATE telemetry`,
		`TRUNCATE logs`,
		`UPDATE channels SET default_version = NULL`,
		`DELETE FROM faas_functions`, // FK CASCADE drops device_render_binding + faas_frame_lastgood
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

	found, err := Delete(ctx, pool, "dev1", false)
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

// T11 (D22.11/K2) — the device-delete telemetry+logs purge is FLAG-GATED, both directions. Flag ON → the
// serial's telemetry AND logs rows are dropped in the same tx (both-or-neither); flag OFF (default) → the
// rows SURVIVE (retention-managed), which is exactly the data-bleed window the flag exists to close.
// Red: (a) an unconditional purge destroys rows a retention/forensics policy required to outlive the
// delete; (b) flag-off lets a re-registered same serial inherit the old device's telemetry as its own.
func TestDelete_TelemetryPurge_FlagGated_T11(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	seed := func(serial string) {
		mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ($1,'stable')`, serial)
		mustExec(t, pool, `INSERT INTO telemetry (time, serial, batt_pct) VALUES (now(),$1,50)`, serial)
		mustExec(t, pool, `INSERT INTO logs (time, serial, payload) VALUES (now(),$1,'hello')`, serial)
	}
	count := func(table, serial string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE serial=$1`, serial).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// flag OFF (default): the time-series rows SURVIVE the delete.
	seed("dev-keep")
	if found, err := Delete(ctx, pool, "dev-keep", false); err != nil || !found {
		t.Fatalf("delete dev-keep: found=%v err=%v", found, err)
	}
	if got := count("telemetry", "dev-keep"); got != 1 {
		t.Errorf("flag-off: telemetry must survive the delete; got %d, want 1", got)
	}
	if got := count("logs", "dev-keep"); got != 1 {
		t.Errorf("flag-off: logs must survive the delete; got %d, want 1", got)
	}

	// flag ON: the time-series rows are purged in the SAME tx as the control-plane delete.
	seed("dev-purge")
	if found, err := Delete(ctx, pool, "dev-purge", true); err != nil || !found {
		t.Fatalf("delete dev-purge: found=%v err=%v", found, err)
	}
	if got := count("telemetry", "dev-purge"); got != 0 {
		t.Errorf("flag-on: telemetry must be purged; got %d, want 0", got)
	}
	if got := count("logs", "dev-purge"); got != 0 {
		t.Errorf("flag-on: logs must be purged; got %d, want 0", got)
	}
}

// T11-del (A24 K2) — deleting a device clears its FaaS render binding AND per-(serial,fn) last-good IN THE
// SAME control-plane tx, ALWAYS (not flag-gated: a binding is control-plane state, not retention time-
// series). Red: the binding/last-good survive → a re-onboarded same serial silently inherits the old
// device's bound function and stale frame (the D17.4 re-bond bleed, FaaS half). The bound function itself
// and OTHER devices' bindings must survive — the delete is serial-scoped, not a function purge.
func TestDelete_CascadesFaasBindings_T11del(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blank := make([]byte, 30000) // faas_lastgood_len CHECK = exactly 30000 bytes

	var fnID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO faas_functions (name, source) VALUES ('fn-del','//src') RETURNING id`).Scan(&fnID); err != nil {
		t.Fatalf("insert function: %v", err)
	}
	for _, serial := range []string{"dev-gone", "dev-stay"} {
		mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ($1,'stable')`, serial)
		mustExec(t, pool, `INSERT INTO device_render_binding (serial, function_id) VALUES ($1,$2)`, serial, fnID)
		mustExec(t, pool, `INSERT INTO faas_frame_lastgood (serial, function_id, packed, status) VALUES ($1,$2,$3,'ok')`, serial, fnID, blank)
	}

	found, err := Delete(ctx, pool, "dev-gone", false)
	if err != nil || !found {
		t.Fatalf("delete dev-gone: found=%v err=%v", found, err)
	}

	count := func(table, serial string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE serial=$1`, serial).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if n := count("device_render_binding", "dev-gone"); n != 0 {
		t.Errorf("deleted device's render binding survived (%d rows)", n)
	}
	if n := count("faas_frame_lastgood", "dev-gone"); n != 0 {
		t.Errorf("deleted device's last-good survived (%d rows)", n)
	}
	// serial-scoped: the other device's binding + last-good AND the function itself all survive.
	if n := count("device_render_binding", "dev-stay"); n != 1 {
		t.Errorf("bystander device's binding was wrongly cleared (%d rows, want 1)", n)
	}
	if n := count("faas_frame_lastgood", "dev-stay"); n != 1 {
		t.Errorf("bystander device's last-good was wrongly cleared (%d rows, want 1)", n)
	}
	var fnStill int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM faas_functions WHERE id=$1`, fnID).Scan(&fnStill); err != nil {
		t.Fatal(err)
	}
	if fnStill != 1 {
		t.Errorf("the bound function was wrongly deleted (%d rows, want 1)", fnStill)
	}
}
