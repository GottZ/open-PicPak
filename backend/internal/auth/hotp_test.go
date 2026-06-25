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

// TestValidate covers the fail-closed states as negative-probed checks: replay, wrong OTP, spoof,
// unprovisioned, and beyond-window MUST be rejected; bootstrap / normal / bounded recovery accepted.
func TestValidate(t *testing.T) {
	key := []byte("12345678901234567890")
	cfg := Config{Digits: 6, Window: 8, WindowFar: 4096}
	bc := uint64(5)
	base := bc << KSplit // boot_count=5, rtc=0
	otp := func(c uint64) uint32 { return HOTP(key, c, cfg.Digits) }

	// bootstrap: first contact accepts the explicit composite
	if r := Validate(cfg, DeviceState{Secret: key, LastCounter: 0}, base, bc, otp(base)); !r.OK || r.NewCounter != base {
		t.Fatalf("bootstrap should accept: %+v", r)
	}

	dev := DeviceState{Secret: key, LastCounter: base, Bootstrapped: true}
	if Validate(cfg, dev, base, bc, otp(base)).OK {
		t.Error("replay (same counter) must reject")
	}
	if r := Validate(cfg, dev, base+1, bc, otp(base+1)); !r.OK {
		t.Errorf("normal next counter should accept: %+v", r)
	}
	if Validate(cfg, dev, base+1, bc, otp(base+2)).OK {
		t.Error("wrong OTP must reject")
	}
	if Validate(cfg, dev, (bc+1)<<KSplit, bc, otp((bc+1)<<KSplit)).OK {
		t.Error("c/bc mismatch (spoof) must reject")
	}
	if Validate(cfg, DeviceState{Secret: nil}, base+1, bc, otp(base+1)).OK {
		t.Error("unprovisioned device must reject")
	}
	if r := Validate(cfg, dev, base+100, bc, otp(base+100)); !r.OK {
		t.Errorf("bounded recovery (within WINDOW_FAR) should accept: %+v", r)
	}
	if Validate(cfg, dev, base+5000, bc, otp(base+5000)).OK {
		t.Error("jump beyond WINDOW_FAR must reject")
	}
}
