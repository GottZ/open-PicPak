package auth

// Shared auth config + result types for the C2 session HOTP validation (see session.go). The legacy
// boot_count-composite validator (Validate / DeviceState / KSplit) was removed at the Doc-15 cutover:
// devices authenticate only via the per-session secret established by the signed ECDSA re-key, so the
// composite counter (and its bc-lockout) no longer exists.

// Config carries the agreed protocol constants. These are policy values, set from server config.
type Config struct {
	Digits    int    // committed DB default is 8; kept configurable per device
	Window    uint64 // normal rtc resync window (e.g. 8)
	WindowFar uint64 // session bootstrap / gap look-ahead bound (e.g. 4096)
}

// Result is the validation outcome. Reason is for metrics/logs ONLY; never returned to the device
// (the device always gets a constant 401 on any failure, to avoid replay/spoof/existence oracles).
type Result struct {
	OK         bool
	NewCounter uint64
	Reason     string
}
