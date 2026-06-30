package rolloutadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- DB property tests (skipped unless TEST_DATABASE_URL is set; run in the e2e gate) ---

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

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func countBin(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

// T5 — register re-hash: a blob whose bytes don't match the claimed sha is rejected with NOTHING
// persisted (no row AND no blob), because the re-hash precedes both the INSERT and the blob write.
// T6 — duplicate version: re-registering an existing version is 23505, blob/sha unchanged.
func TestRegisterFirmware_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blob := []byte("firmware-bytes-v1")
	realSha := sha256hex(blob)

	t.Run("happy path writes row + content-addressed blob", func(t *testing.T) {
		dir := t.TempDir()
		gotSha, err := RegisterFirmware(ctx, pool, dir, "1.0.0", realSha, blob)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if gotSha != realSha {
			t.Errorf("returned sha %q, want %q", gotSha, realSha)
		}
		// row matches the server-computed sha + counted size
		var sha, blobPath string
		var size int64
		mustScan(t, pool, `SELECT sha256, blob_path, size_bytes FROM firmware_versions WHERE version='1.0.0'`,
			&sha, &blobPath, &size)
		if sha != realSha || size != int64(len(blob)) || blobPath != realSha+".bin" {
			t.Errorf("row = (%s,%s,%d), want (%s,%s.bin,%d)", sha, blobPath, size, realSha, realSha, len(blob))
		}
		// blob persisted with the content-addressed name, exact bytes
		on, err := os.ReadFile(dir + "/" + blobPath)
		if err != nil || string(on) != string(blob) {
			t.Errorf("blob file: bytes=%q err=%v", on, err)
		}
	})

	t.Run("T5 sha mismatch -> no row AND no blob", func(t *testing.T) {
		mustExec(t, pool, `DELETE FROM firmware_versions`)
		dir := t.TempDir()
		_, err := RegisterFirmware(ctx, pool, dir, "2.0.0", sha256hex([]byte("a-different-claim")), blob)
		if !errors.Is(err, ErrShaMismatch) {
			t.Fatalf("err = %v, want ErrShaMismatch", err)
		}
		var n int
		mustScan(t, pool, `SELECT count(*) FROM firmware_versions WHERE version='2.0.0'`, &n)
		if n != 0 {
			t.Error("a row was inserted on a sha mismatch")
		}
		if c := countBin(t, dir); c != 0 {
			t.Errorf("%d blob file(s) written on a sha mismatch, want 0", c)
		}
	})

	t.Run("T6 duplicate version -> 23505", func(t *testing.T) {
		mustExec(t, pool, `DELETE FROM firmware_versions`)
		dir := t.TempDir()
		if _, err := RegisterFirmware(ctx, pool, dir, "3.0.0", realSha, blob); err != nil {
			t.Fatalf("first register: %v", err)
		}
		_, err := RegisterFirmware(ctx, pool, dir, "3.0.0", realSha, blob)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			t.Fatalf("dup err = %v, want unique_violation 23505", err)
		}
	})
}

func TestSetChannelDefault_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ('v1',$1,'v1.bin',1)`, sha256hex([]byte("x")))

	found, err := SetChannelDefault(ctx, pool, "stable", "v1")
	if err != nil || !found {
		t.Fatalf("set stable=v1: found=%v err=%v", found, err)
	}
	found, err = SetChannelDefault(ctx, pool, "ghost-channel", "v1")
	if err != nil || found {
		t.Errorf("unknown channel: found=%v err=%v, want found=false no err", found, err)
	}
	_, err = SetChannelDefault(ctx, pool, "stable", "ghost-version")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("unknown version err = %v, want FK 23503", err)
	}
}

func TestUpsertAndStateAndDelete_DB(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ('v1',$1,'v1.bin',1),('v2',$1,'v2.bin',1)`, sha256hex([]byte("x")))
	mustExec(t, pool, `INSERT INTO devices (serial, channel) VALUES ('dev1','stable')`)

	// SerialResolvable: '*' and a real device pass; an unknown serial fails.
	for _, c := range []struct {
		serial string
		want   bool
	}{{"*", true}, {"dev1", true}, {"ghost", false}} {
		ok, err := SerialResolvable(ctx, pool, c.serial)
		if err != nil || ok != c.want {
			t.Errorf("SerialResolvable(%q) = %v (err %v), want %v", c.serial, ok, err, c.want)
		}
	}

	// upsert creates, then re-upsert on the same (serial,channel) updates version + reactivates.
	id, err := UpsertRollout(ctx, pool, "dev1", "stable", "v1")
	if err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	mustExec(t, pool, `UPDATE rollout_targets SET state='paused' WHERE id=$1`, id)
	id2, err := UpsertRollout(ctx, pool, "dev1", "stable", "v2")
	if err != nil || id2 != id {
		t.Fatalf("re-upsert: id=%d err=%v, want same id %d", id2, err, id)
	}
	var ver, state string
	if err := pool.QueryRow(ctx, `SELECT version, state FROM rollout_targets WHERE id=$1`, id).Scan(&ver, &state); err != nil {
		t.Fatalf("scan rollout %d: %v", id, err)
	}
	if ver != "v2" || state != "active" {
		t.Errorf("after re-upsert = (%s,%s), want (v2,active)", ver, state)
	}

	// state + delete found semantics
	if found, _ := SetRolloutState(ctx, pool, id, "done"); !found {
		t.Error("SetRolloutState existing -> found=false")
	}
	if found, _ := SetRolloutState(ctx, pool, 999999, "done"); found {
		t.Error("SetRolloutState missing -> found=true")
	}
	if found, _ := DeleteRollout(ctx, pool, id); !found {
		t.Error("DeleteRollout existing -> found=false")
	}
	if found, _ := DeleteRollout(ctx, pool, id); found {
		t.Error("DeleteRollout already-gone -> found=true")
	}

	// unknown channel/version on upsert -> FK 23503
	_, err = UpsertRollout(ctx, pool, "dev1", "ghost-channel", "v1")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("unknown channel upsert err = %v, want FK 23503", err)
	}
}

func mustScan(t *testing.T, pool *pgxpool.Pool, sql string, dst ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql).Scan(dst...); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}
