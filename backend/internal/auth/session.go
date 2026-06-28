package auth

// Per-session HOTP validation for the C2 auth re-architecture (Doc 15).
//
// The session secret is established out-of-band via the signed Ed25519 re-key handshake; the device
// holds it (and its counter) only in RTC-RAM. Within a session the counter is a FLAT, free-running
// monotonic value — there is NO boot_count composite, so the 2^KSplit epoch jump (and thus the
// bc-lockout) cannot occur. A boot_count change on the device wipes the RTC-RAM session, which the
// device resolves by re-keying (a fresh secret + a counter reset on both sides), not by trying to
// bridge a counter jump.

// ValidateSession runs the fail-closed HOTP check against the current session secret + counter. Pure
// logic: the caller holds the per-device row lock and persists NewCounter (+ bootstrapped) only on OK.
// secret == nil means no session is established yet -> the device must re-key first.
func ValidateSession(cfg Config, secret []byte, lastCounter uint64, bootstrapped bool, c uint64, otp uint32) Result {
	if secret == nil {
		return Result{Reason: "no_session"}
	}
	match := func(x uint64) bool { return HOTP(secret, x, cfg.Digits) == otp }
	switch {
	case !bootstrapped:
		// first poll of a fresh session: the device starts its counter low (reset on re-key), so a
		// match within a window is the proof. No unbounded accept (the device controls the start).
		if c <= cfg.WindowFar && match(c) {
			return Result{OK: true, NewCounter: c, Reason: "session-bootstrap"}
		}
	case c <= lastCounter:
		return Result{Reason: "replay"} // strictly monotone within the session
	case c <= lastCounter+cfg.WindowFar:
		// the gap absorbs polls whose response the device never saw (its counter advanced, ours did
		// not); bounded by WindowFar. A counter from a stale (pre-re-key) session simply will not
		// match the current secret -> 401 -> the device re-keys.
		if match(c) {
			return Result{OK: true, NewCounter: c, Reason: "session"}
		}
	}
	return Result{Reason: "no_match"}
}
