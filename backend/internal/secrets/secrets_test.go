package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/backend/internal/sealbox"
)

// --- DB-free policy ---.

func TestValidSecretName(t *testing.T) {
	valid := []string{"a", "x0", "openrouter-main", "home.assistant_token", "a.b-c_d", strings.Repeat("a", 128)}
	for _, n := range valid {
		if !ValidSecretName(n) {
			t.Errorf("rejected valid name %q", n)
		}
	}
	invalid := []string{
		"",                       // empty
		"A",                      // uppercase
		"-lead",                  // must start alnum
		".lead",                  // must start alnum
		"_lead",                  // must start alnum
		"has space",              // space
		"has:colon",              // ':' would break AAD/DecodeLine
		"has/slash",              // charset
		"ünïcode",                // non-ascii
		strings.Repeat("a", 129), // too long
	}
	for _, n := range invalid {
		if ValidSecretName(n) {
			t.Errorf("accepted invalid name %q", n)
		}
	}
}

// --- DB property tests (skipped unless TEST_DATABASE_URL is set; run in the e2e gate) ---.

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
	if _, err := pool.Exec(context.Background(), `TRUNCATE secrets`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func testBox(t *testing.T, currentHex, prevHex string) *sealbox.Box {
	t.Helper()
	b, err := sealbox.New(currentHex, prevHex)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	return b
}

const (
	keyA = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0" // 64 hex = 32 bytes
	keyB = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
)

func TestPutResolve_RoundTrip(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")

	if _, err := ResolveSecret(ctx, pool, b, "absent"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing name: want ErrSecretNotFound, got %v", err)
	}

	value := []byte("PLAINTEXTMARKER-do-not-leak")
	created, err := PutSecret(ctx, pool, b, "api-key", value)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if !created {
		t.Error("first PutSecret: created=false, want true")
	}
	got, err := ResolveSecret(ctx, pool, b, "api-key")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got) != string(value) {
		t.Error("roundtrip value mismatch")
	}
}

func TestPut_RotateSetsRotatedAt(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")

	if _, err := PutSecret(ctx, pool, b, "rot", []byte("v1")); err != nil {
		t.Fatalf("put1: %v", err)
	}
	created, err := PutSecret(ctx, pool, b, "rot", []byte("v2-updated"))
	if err != nil {
		t.Fatalf("put2: %v", err)
	}
	if created {
		t.Error("second PutSecret: created=true, want false (rotate)")
	}
	m, err := GetMeta(ctx, pool, "rot")
	if err != nil {
		t.Fatalf("getmeta: %v", err)
	}
	if m.RotatedAt == nil {
		t.Error("rotated_at is nil after rotate")
	}
	got, err := ResolveSecret(ctx, pool, b, "rot")
	if err != nil || string(got) != "v2-updated" {
		t.Errorf("resolve after rotate = %q, %v", got, err)
	}
}

// T6: metadata never carries the value or ciphertext — the JSON the API would emit is clean.
func TestMeta_NeverEchoesValue(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")
	marker := "SUPERSECRETVALUE-1234567890"
	if _, err := PutSecret(ctx, pool, b, "leaky", []byte(marker)); err != nil {
		t.Fatalf("put: %v", err)
	}
	one, err := GetMeta(ctx, pool, "leaky")
	if err != nil {
		t.Fatalf("getmeta: %v", err)
	}
	list, err := ListMeta(ctx, pool)
	if err != nil {
		t.Fatalf("listmeta: %v", err)
	}
	blob, _ := json.Marshal(struct {
		One  Meta
		List []Meta
	}{one, list})
	if strings.Contains(string(blob), marker) {
		t.Errorf("metadata JSON leaks the value: %s", blob)
	}
	if strings.Contains(string(blob), "ciphertext") || strings.Contains(string(blob), "nonce") {
		t.Errorf("metadata JSON exposes ciphertext/nonce fields: %s", blob)
	}
	if len(list) != 1 || list[0].Name != "leaky" {
		t.Errorf("list = %+v", list)
	}
}

// T12: ResolveSecret splits the two failure classes — a missing row is ErrSecretNotFound (fail-open
// eligible), but a tampered ciphertext propagates a HARD sealbox error, NOT ErrSecretNotFound.
func TestResolve_TamperedRowHardError(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")
	if _, err := PutSecret(ctx, pool, b, "tampered", []byte("real value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	// flip bit 0 of ciphertext byte 0 directly in the row
	if _, err := pool.Exec(ctx,
		`UPDATE secrets SET ciphertext = set_byte(ciphertext, 0, get_byte(ciphertext, 0) # 1) WHERE name = 'tampered'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err := ResolveSecret(ctx, pool, b, "tampered")
	if err == nil {
		t.Fatal("tampered row resolved without error")
	}
	if errors.Is(err, ErrSecretNotFound) {
		t.Error("tampered ciphertext masked as ErrSecretNotFound — fail-open would hide DB tampering")
	}
}

func TestDeleteSecret(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")
	if _, err := PutSecret(ctx, pool, b, "doomed", []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}
	found, err := DeleteSecret(ctx, pool, "doomed")
	if err != nil || !found {
		t.Errorf("delete existing: found=%v err=%v", found, err)
	}
	found, err = DeleteSecret(ctx, pool, "doomed")
	if err != nil || found {
		t.Errorf("delete absent: found=%v err=%v (want false)", found, err)
	}
}

// T5: the rotation sweep re-seals prev-key rows under the current key and bumps key_version, after
// which the row opens under the current key alone; a second sweep is a no-op.
func TestSweep_PrevKeyReseal(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	// seal under key A
	boxA := testBox(t, keyA, "")
	if _, err := PutSecret(ctx, pool, boxA, "rotme", []byte("rotation payload")); err != nil {
		t.Fatalf("put: %v", err)
	}

	// rotation window: current=B, prev=A
	boxAB := testBox(t, keyB, keyA)
	n, err := Sweep(ctx, pool, boxAB)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("re-sealed %d, want 1", n)
	}
	m, err := GetMeta(ctx, pool, "rotme")
	if err != nil {
		t.Fatalf("getmeta: %v", err)
	}
	if m.KeyVersion != 2 {
		t.Errorf("key_version = %d, want 2 after sweep", m.KeyVersion)
	}

	// now readable under current key B alone (prev dropped)
	boxB := testBox(t, keyB, "")
	got, err := ResolveSecret(ctx, pool, boxB, "rotme")
	if err != nil || string(got) != "rotation payload" {
		t.Errorf("resolve under current = %q, %v", got, err)
	}

	// second sweep: nothing left under prev
	n2, err := Sweep(ctx, pool, boxAB)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep re-sealed %d, want 0", n2)
	}
}

func TestSweep_NoPrevIsNoop(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	b := testBox(t, keyA, "")
	if _, err := PutSecret(ctx, pool, b, "x", []byte("v")); err != nil {
		t.Fatalf("put: %v", err)
	}
	n, err := Sweep(ctx, pool, b)
	if err != nil || n != 0 {
		t.Errorf("no-prev sweep: n=%d err=%v, want 0/nil", n, err)
	}
}
