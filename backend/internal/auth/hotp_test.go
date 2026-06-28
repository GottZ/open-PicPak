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
	cfg := Config{Digits: 8, Window: 8, WindowFar: 4096}
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

	// --- epoch advance (the field bc-lockout regression guard) ---
	// A boot_count increment (every deep-sleep wake / reset) resets rtc -> the composite jumps by
	// 2^KSplit, far beyond WindowFar, but is a legitimate forward step with c and bc BOTH advanced
	// consistently. MUST accept (this is exactly what the live device sends after its first reboot).
	newEpoch := ((bc + 1) << KSplit) | 1
	if r := Validate(cfg, dev, newEpoch, bc+1, otp(newEpoch)); !r.OK || r.NewCounter != newEpoch {
		t.Errorf("epoch advance (bc++) must accept (field lockout bug): %+v", r)
	}
	// A multi-reboot gap (several boots while offline, no successful auth) still accepts the first
	// authed poll on return.
	gapEpoch := ((bc + 7) << KSplit) | 1
	if r := Validate(cfg, dev, gapEpoch, bc+7, otp(gapEpoch)); !r.OK {
		t.Errorf("multi-reboot epoch gap must accept: %+v", r)
	}
	// But a new epoch with a WRONG OTP must still reject (the secret is still required).
	if Validate(cfg, dev, newEpoch, bc+1, otp(newEpoch+1)).OK {
		t.Error("epoch advance with wrong OTP must reject")
	}
	// And a cross-epoch replay (an old epoch's counter, now behind a newer LastCounter) must reject.
	devAdvanced := DeviceState{Secret: key, LastCounter: newEpoch, Bootstrapped: true}
	if Validate(cfg, devAdvanced, base+1, bc, otp(base+1)).OK {
		t.Error("cross-epoch replay (old epoch counter) must reject")
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
