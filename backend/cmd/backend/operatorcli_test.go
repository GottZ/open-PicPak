package main

import (
	"testing"

	"github.com/open-picpak/backend/internal/operator"
)

// A minted token is random, and its stored hash is exactly sha256(printed token) so the bearer
// authenticates later. The plaintext is the only thing ever shown; only the 32-byte hash persists.
func TestNewOperatorToken(t *testing.T) {
	t1, h1 := newOperatorToken()
	t2, _ := newOperatorToken()
	if t1 == t2 {
		t.Fatal("two minted tokens identical — randomness broken")
	}
	if len(h1) != 32 {
		t.Fatalf("hash must be 32 bytes (operator_keys CHECK), got %d", len(h1))
	}
	if got := operator.HashToken(t1); string(got) != string(h1) {
		t.Fatal("stored hash is not sha256(printed token) — bearer would never authenticate")
	}
	if len(t1) < 40 {
		t.Fatalf("token suspiciously short (%d) — want ~43 chars (32B base64url)", len(t1))
	}
}
