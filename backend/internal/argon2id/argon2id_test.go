package argon2id

import (
	"strings"
	"testing"
)

// TestHashVerifyRoundTrip is DB-independent (-short): a hashed password verifies, and a wrong
// password does not. This is the structural half of the admin_users negative probe (c).
func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Fatalf("unexpected PHC prefix: %q", phc)
	}
	// The plaintext must not appear anywhere in the encoded hash.
	if strings.Contains(phc, "correct horse") {
		t.Fatalf("plaintext leaked into PHC string: %q", phc)
	}
	ok, err := Verify("correct horse battery staple", phc)
	if err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	ok, err = Verify("wrong password", phc)
	if err != nil {
		t.Fatalf("verify wrong: unexpected err=%v", err)
	}
	if ok {
		t.Fatal("verify accepted a wrong password")
	}
}

// TestVerifyRejectsMalformed: a corrupt PHC string is a clean error, not a panic or a false-accept.
func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "not-a-phc", "$argon2id$v=19$m=1$onlyfourparts", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA"} {
		if ok, err := Verify("x", bad); ok || err == nil {
			t.Fatalf("Verify(%q): want (false, err), got (%v, %v)", bad, ok, err)
		}
	}
}
