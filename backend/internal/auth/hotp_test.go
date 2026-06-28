package auth

import "testing"

// TestHOTP_RFC4226 reproduces the canonical RFC 4226 Appendix D vectors: the same vectors the
// firmware core passes (firmware/test/test_auth.c). Both green means FW and server HOTP match.
func TestHOTP_RFC4226(t *testing.T) {
	key := []byte("12345678901234567890")
	want6 := []uint32{755224, 287082, 359152, 969429, 338314, 254676, 287922, 162583, 399871, 520489}
	want8 := []uint32{84755224, 94287082, 37359152, 26969429, 40338314, 68254676, 18287922, 82162583, 73399871, 45520489}
	for c := uint64(0); c < 10; c++ {
		if got := HOTP(key, c, 6); got != want6[c] {
			t.Errorf("HOTP6(%d) = %d, want %d", c, got, want6[c])
		}
		if got := HOTP(key, c, 8); got != want8[c] {
			t.Errorf("HOTP8(%d) = %d, want %d", c, got, want8[c])
		}
	}
	if HOTP(key, 0, 5) != 0 || HOTP(key, 0, 9) != 0 {
		t.Error("digits outside [6,8] must return 0 (mirrors firmware guard)")
	}
}

// TestValidateSession covers the Doc-15 flat session counter (no boot_count composite): bootstrap,
// monotonic accept, replay/wrong-otp/no-session/beyond-window reject. A boot_count change never
// appears here (the device re-keys instead), so there is no epoch-jump to handle.
func TestValidateSession(t *testing.T) {
	s := []byte("12345678901234567890")
	cfg := Config{Digits: 8, Window: 8, WindowFar: 4096}
	otp := func(c uint64) uint32 { return HOTP(s, c, cfg.Digits) }

	// no session established yet -> reject (device must re-key first)
	if ValidateSession(cfg, nil, 0, false, 1, otp(1)).OK {
		t.Error("no session secret must reject")
	}
	// bootstrap: first poll of a fresh session (counter starts low)
	if r := ValidateSession(cfg, s, 0, false, 1, otp(1)); !r.OK || r.NewCounter != 1 {
		t.Fatalf("session bootstrap should accept: %+v", r)
	}
	// established session at lastCounter=10
	if ValidateSession(cfg, s, 10, true, 10, otp(10)).OK {
		t.Error("session replay (same counter) must reject")
	}
	if ValidateSession(cfg, s, 10, true, 5, otp(5)).OK {
		t.Error("session replay (lower counter) must reject")
	}
	if r := ValidateSession(cfg, s, 10, true, 11, otp(11)); !r.OK || r.NewCounter != 11 {
		t.Errorf("session monotonic next should accept: %+v", r)
	}
	if r := ValidateSession(cfg, s, 10, true, 100, otp(100)); !r.OK {
		t.Errorf("session gap within window should accept: %+v", r)
	}
	if ValidateSession(cfg, s, 10, true, 11, otp(12)).OK {
		t.Error("session wrong OTP must reject")
	}
	if ValidateSession(cfg, s, 10, true, 10+5000, otp(10+5000)).OK {
		t.Error("session jump beyond window must reject")
	}
	// a stale-session counter (would match a DIFFERENT secret) must not validate against this secret
	other := []byte("09876543210987654321")
	if ValidateSession(cfg, s, 10, true, 11, HOTP(other, 11, cfg.Digits)).OK {
		t.Error("otp from a different (stale-session) secret must reject")
	}
}
