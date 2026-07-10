package apitoken

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run against an ephemeral postgres
// with migrations 0001..0011 + 0014 applied). Mirrors imgstore/store_test.go.
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
	if _, err := pool.Exec(context.Background(), `TRUNCATE api_tokens RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// TestCreateNeverStoresPlaintext is the api_tokens negative probe (a): after Create, the plaintext
// secret must NOT be recoverable from the DB — no ppk_ prefix in storage, and secret_hash must be
// sha256(secret), never the secret bytes themselves. Red state: a naive plaintext column.
func TestCreateNeverStoresPlaintext(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	plaintext, tok, err := Create(ctx, pool, "ci-uploader", []string{"image:write"}, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		t.Fatalf("plaintext missing ppk_ prefix: %q", plaintext)
	}
	_, secret, ok := ParseToken(plaintext)
	if !ok {
		t.Fatalf("ParseToken failed for %q", plaintext)
	}

	// No text column anywhere in the row may contain the plaintext token or its secret part.
	var hitPlaintext, hitSecret int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM api_tokens WHERE token_id = $1 OR label = $1`, plaintext).Scan(&hitPlaintext); err != nil {
		t.Fatalf("scan plaintext hits: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM api_tokens WHERE token_id = $1 OR label = $1`, secret).Scan(&hitSecret); err != nil {
		t.Fatalf("scan secret hits: %v", err)
	}
	if hitPlaintext != 0 || hitSecret != 0 {
		t.Fatalf("plaintext/secret found in a text column: plaintext=%d secret=%d", hitPlaintext, hitSecret)
	}

	// secret_hash must equal sha256(secret) and must NOT equal the raw secret bytes.
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT secret_hash FROM api_tokens WHERE id = $1`, tok.ID).Scan(&storedHash); err != nil {
		t.Fatalf("scan secret_hash: %v", err)
	}
	want := sha256.Sum256([]byte(secret))
	if string(storedHash) != string(want[:]) {
		t.Fatalf("secret_hash is not sha256(secret)")
	}
	if string(storedHash) == secret {
		t.Fatalf("secret_hash stored the plaintext secret")
	}
}

// TestResolveConstantTimeCompare is the api_tokens negative probe (b): a correct secret resolves;
// a wrong secret under the SAME token_id does not (constant-time compare, never a match). Also
// covers disabled and expired tokens resolving as absent.
func TestResolveConstantTimeCompare(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	plaintext, tok, err := Create(ctx, pool, "reader", []string{"image:read"}, nil, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tokenID, secret, _ := ParseToken(plaintext)

	// Correct secret resolves.
	got, ok, err := Resolve(ctx, pool, plaintext)
	if err != nil || !ok || got.ID != tok.ID {
		t.Fatalf("resolve correct: ok=%v id=%d err=%v", ok, got.ID, err)
	}

	// Wrong secret under the same (valid) token_id must NOT resolve. Flip the last char to one
	// guaranteed different from the original (a fixed 'A' collides ~6-16% of the time, since the
	// last base64url char of a 32-byte value is restricted — a real forged token would then equal
	// the real one and spuriously resolve).
	repl := byte('A')
	if secret[len(secret)-1] == 'A' {
		repl = 'B'
	}
	forged := TokenPrefix + tokenID + "_" + secret[:len(secret)-1] + string(repl)
	if _, ok, err := Resolve(ctx, pool, forged); ok || err != nil {
		t.Fatalf("resolve forged secret: want (false,nil), got (%v,%v)", ok, err)
	}

	// Unknown token_id, malformed, and non-ppk bearer all resolve as absent.
	for _, bad := range []string{TokenPrefix + "unknownid_" + secret, "not-a-token", "Bearer x"} {
		if _, ok, err := Resolve(ctx, pool, bad); ok || err != nil {
			t.Fatalf("resolve %q: want (false,nil), got (%v,%v)", bad, ok, err)
		}
	}

	// Disabled token resolves as absent even with the correct secret.
	if err := Revoke(ctx, pool, tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok, err := Resolve(ctx, pool, plaintext); ok || err != nil {
		t.Fatalf("resolve disabled: want (false,nil), got (%v,%v)", ok, err)
	}
}

// TestResolveExpired: an expired token does not resolve.
func TestResolveExpired(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	plaintext, _, err := Create(ctx, pool, "expired", []string{"image:read"}, &past, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok, err := Resolve(ctx, pool, plaintext); ok || err != nil {
		t.Fatalf("resolve expired: want (false,nil), got (%v,%v)", ok, err)
	}
}

// TestCreateRejectsUnknownScope: an out-of-allowlist scope is a clean app-side error, not a raw
// CHECK violation.
func TestCreateRejectsUnknownScope(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	if _, _, err := Create(ctx, pool, "bad", []string{"fleet:admin"}, nil, nil); err == nil {
		t.Fatal("Create accepted an unknown scope")
	}
}

// TestParseTokenUnderscoreInParts is the deterministic regression for the id/secret separator bug:
// both token_id and secret are base64url and may contain '_', so ParseToken must split at the fixed
// token_id length, not the first '_'. Red state (strings.Cut on first '_'): the token_id below is
// truncated to "aa" and the DB lookup misses → a valid token 401s ~21% of the time.
func TestParseTokenUnderscoreInParts(t *testing.T) {
	id := "aa_defghijklmno1"                             // 16 chars, contains '_'
	secret := "s_cret_with_many_underscores_1234567890abcd" // 43 chars, contains '_'
	if len(id) != 16 || len(secret) != 43 {
		t.Fatalf("test fixture lengths wrong: id=%d secret=%d", len(id), len(secret))
	}
	raw := TokenPrefix + id + "_" + secret
	gotID, gotSecret, ok := ParseToken(raw)
	if !ok || gotID != id || gotSecret != secret {
		t.Fatalf("ParseToken split wrong: ok=%v id=%q secret=%q (want id=%q secret=%q)", ok, gotID, gotSecret, id, secret)
	}
}

// TestParseTokenRoundTripsCreate: every freshly minted token parses back to a non-empty id+secret,
// regardless of which base64url chars the RNG produced (a probabilistic guard on the fix).
func TestParseTokenRoundTrips(t *testing.T) {
	for i := 0; i < 500; i++ {
		id, err := randString(tokenIDLen)
		if err != nil {
			t.Fatal(err)
		}
		secret, err := randString(secretLen)
		if err != nil {
			t.Fatal(err)
		}
		raw := TokenPrefix + id + "_" + secret
		gotID, gotSecret, ok := ParseToken(raw)
		if !ok || gotID != id || gotSecret != secret {
			t.Fatalf("round-trip %d failed: raw=%q -> ok=%v id=%q secret=%q", i, raw, ok, gotID, gotSecret)
		}
	}
}
