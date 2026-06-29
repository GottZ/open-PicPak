package devicestore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
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
