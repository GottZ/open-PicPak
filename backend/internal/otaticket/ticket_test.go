package otaticket

import (
	"strings"
	"testing"
	"time"
)

const testKey = "0123456789abcdef0123456789abcdef" // 32 bytes — a strong key for the happy path

// Round-trip: a freshly minted ticket verifies and returns the exact (sn, version), including a
// version with the awkward characters esp_app_desc can carry ('|', '.', space) — the b64url framing
// must survive them (D20.4 injectivity).
func TestMintVerifyRoundTrip(t *testing.T) {
	cases := []struct{ sn, version string }{
		{"dev1", "1.2.3"},
		{"AB12CD3", "1.2.3|rc.4 (dirty)"}, // '|' + '.' + space in version
		{"x", ""},                          // empty version
	}
	for _, c := range cases {
		tok := Mint(testKey, c.sn, c.version, time.Minute)
		sn, ver, err := Verify(testKey, tok)
		if err != nil {
			t.Fatalf("Verify(%q/%q) err: %v", c.sn, c.version, err)
		}
		if sn != c.sn || ver != c.version {
			t.Errorf("round-trip = (%q,%q), want (%q,%q)", sn, ver, c.sn, c.version)
		}
	}
}

// T1 (unit half) — no/garbage ticket: an empty or wrong-shaped token is malformed, never decoded into
// a usable (sn,version). The ingest handler maps this to 403 (anonymous binary blocked).
func TestVerifyMalformed(t *testing.T) {
	for _, tok := range []string{"", "a.b.c", "a.b.c.d.e", "onlyonepart", "a..d"} {
		if _, _, err := Verify(testKey, tok); err == nil {
			t.Errorf("Verify(%q) = nil err, want malformed/bad", tok)
		}
	}
}

// T2 — expired ticket: exp in the past → ErrExpired (red: a captured ticket streams forever).
func TestVerifyExpired(t *testing.T) {
	tok := Mint(testKey, "dev1", "1.2.3", -1*time.Second) // already expired
	if _, _, err := Verify(testKey, tok); err != ErrExpired {
		t.Errorf("Verify(expired) = %v, want ErrExpired", err)
	}
}

// T3 — forged/tampered: a flipped MAC, a swapped version segment, segment reorder, or the wrong key
// all fail the MAC check (red: no ConstantTimeCompare/no recompute → the binary serves; also covers
// "pivot the ticket to a different version").
func TestVerifyForged(t *testing.T) {
	good := Mint(testKey, "dev1", "1.2.3", time.Minute)
	parts := strings.Split(good, ".")

	// wrong key
	if _, _, err := Verify("XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX", good); err != ErrBadMAC {
		t.Errorf("wrong key = %v, want ErrBadMAC", err)
	}
	// flipped last char of the MAC segment
	macSeg := []byte(parts[3])
	if macSeg[len(macSeg)-1] == 'A' {
		macSeg[len(macSeg)-1] = 'B'
	} else {
		macSeg[len(macSeg)-1] = 'A'
	}
	tampMAC := parts[0] + "." + parts[1] + "." + parts[2] + "." + string(macSeg)
	if _, _, err := Verify(testKey, tampMAC); err != ErrBadMAC {
		t.Errorf("flipped MAC = %v, want ErrBadMAC", err)
	}
	// swap a different version in, keep the original MAC
	other := Mint(testKey, "dev1", "9.9.9", time.Minute)
	otherVer := strings.Split(other, ".")[1]
	pivot := parts[0] + "." + otherVer + "." + parts[2] + "." + parts[3]
	if _, _, err := Verify(testKey, pivot); err != ErrBadMAC {
		t.Errorf("version-pivot = %v, want ErrBadMAC", err)
	}
	// reorder sn<->version segments (injectivity: the MAC was over the original order)
	swapped := parts[1] + "." + parts[0] + "." + parts[2] + "." + parts[3]
	if _, _, err := Verify(testKey, swapped); err != ErrBadMAC {
		t.Errorf("segment-swap = %v, want ErrBadMAC", err)
	}
}

// D20.9 — the boot gate: a key shorter than 32 bytes (or empty) is not strong enough to sign with;
// cmd/ingest uses this to disable the serve route rather than mint forgeable tickets.
func TestKeyStrong(t *testing.T) {
	if KeyStrong("") || KeyStrong(strings.Repeat("a", 31)) {
		t.Error("weak/empty key reported strong")
	}
	if !KeyStrong(strings.Repeat("a", 32)) || !KeyStrong(strings.Repeat("a", 64)) {
		t.Error("strong key reported weak")
	}
}
