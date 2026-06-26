package auth

// KSplit is the composite-counter bit split: C = (boot_count << KSplit) | rtc_counter.
const KSplit = 24

// Config carries the agreed protocol constants. These are policy values, set from server config.
type Config struct {
	Digits    int    // committed DB default is 8; kept configurable per device
	Window    uint64 // normal rtc resync window (e.g. 8)
	WindowFar uint64 // recovery / bootstrap look-ahead bound (e.g. 4096)
}

// DeviceState is the per-device auth row (loaded under SELECT FOR UPDATE by the caller).
type DeviceState struct {
	Secret       []byte // raw HMAC key; nil => not yet provisioned
	LastCounter  uint64 // last accepted composite C
	Bootstrapped bool   // a first real C has been accepted
}

// Result is the validation outcome. Reason is for metrics/logs ONLY; never returned to the device
// (the device always gets a constant 401 on any failure, to avoid replay/spoof/existence oracles).
type Result struct {
	OK         bool
	NewCounter uint64
	Reason     string
}

// Validate runs the fail-closed HOTP check. Pure logic: the caller holds the
// per-device row lock and persists NewCounter (+ Bootstrapped) only on OK. Because the device sends
// the composite C explicitly, no candidate scan is needed; we validate exactly claimedC, with the
// window only acting as a monotonicity/range guard (replay below, far-future bounded).
func Validate(cfg Config, dev DeviceState, claimedC, bc uint64, otp uint32) Result {
	if dev.Secret == nil {
		return Result{Reason: "unprovisioned"} // M1: legacy path carries it; HOTP route 401s
	}
	if (claimedC >> KSplit) != bc {
		return Result{Reason: "c_bc_mismatch"} // spoof: C and reported boot_count disagree
	}
	match := func(c uint64) bool { return HOTP(dev.Secret, c, cfg.Digits) == otp }

	switch {
	case dev.LastCounter == 0 && !dev.Bootstrapped:
		// bootstrap: bc is already >0 (ota_record_boot ran many boots) so claimedC is far from 0;
		// the OTP match itself is the proof, no window bound.
		if match(claimedC) {
			return Result{OK: true, NewCounter: claimedC, Reason: "bootstrap"}
		}
	case claimedC <= dev.LastCounter:
		return Result{Reason: "replay"} // not strictly monotone
	case claimedC <= dev.LastCounter+cfg.Window:
		if match(claimedC) {
			return Result{OK: true, NewCounter: claimedC, Reason: "normal"}
		}
	case claimedC <= dev.LastCounter+cfg.WindowFar:
		// recovery: long offline block / rtc-composite overflow; bounded by WindowFar.
		if match(claimedC) {
			return Result{OK: true, NewCounter: claimedC, Reason: "recovery"}
		}
	}
	return Result{Reason: "no_match"}
}
