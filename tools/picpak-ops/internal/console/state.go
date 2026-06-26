package console

// ConnState is the console's honest link state. The link onto a PicPak is
// fundamentally intermittent — connecting pulses DTR/RTS and reboots the device, the
// interactive input window is a ~3 s peek outside setup mode, and keep-awake is
// best-effort — so the pane never pretends "up". Every state below is distinct and
// surfaced in the title bar so a quiet link is not mistaken for a dead one.
type ConnState int

const (
	// StateIdle: no session. The pane shows "press connect to reset + attach".
	StateIdle ConnState = iota
	// StateResetting: ssh is opening. Connecting pulses DTR/RTS, so the device is
	// rebooting; the firmware banner / first bytes confirm the attach.
	StateResetting
	// StateAttached: banner or first device bytes seen — the interactive window is
	// live (briefly, outside setup mode).
	StateAttached
	// StateQuiet: a STILL-OPEN ssh stream that fell silent. After the firmware exits
	// its console it uninstalls the interrupt-driven USB driver and output thins to
	// log-only on a link that does NOT re-enumerate (realities §3). Quiet is a live
	// link, NOT an error and NOT a reconnect trigger.
	StateQuiet
	// StateSleeping: a genuine reader EOF / process exit while we expected liveness —
	// the device slept out from under the session (keep-awake lost) and the link
	// actually died (realities §4). Distinct from Quiet (live) and Closed (operator).
	StateSleeping
	// StateReconnecting: a transient drop is being retried after the backoff (which
	// itself resets the device — hence the bounded reconnect + confirm guard).
	StateReconnecting
	// StateClosed: operator/ssh teardown (disconnect key, port release, pane close).
	StateClosed
	// StateError: a transport / start failure produced no usable link.
	StateError
)

// String renders the state for the title bar and diagnostics.
func (s ConnState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateResetting:
		return "resetting"
	case StateAttached:
		return "attached"
	case StateQuiet:
		return "quiet"
	case StateSleeping:
		return "sleeping"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// onSilence is the quiet heuristic: quiet_timeout_ms of silence on a STILL-OPEN ssh
// stream means the firmware exited its console / is running a cycle — the USB link
// stays enumerated and output thins to log-only (realities §3). That is Quiet, NOT
// Error and NOT a reconnect trigger. Only an Attached link can go Quiet.
//
// TODO(on-device): tune quiet_timeout_ms / connect=reset timing. The 2000 ms default
// is an UNVERIFIED heuristic with no firmware constant behind it (the discarded
// "~60 ms" figure had no firmware backing); it must be measured on a real PicPak —
// the on-device gate this wave cannot run.
func onSilence(s ConnState) ConnState {
	if s == StateAttached {
		return StateQuiet
	}
	return s
}

// onData is the live-byte transition: any device output (re)attaches the link. A
// Quiet link that speaks again is Attached; a Resetting link that produced bytes is
// Attached. Other states are left untouched (e.g. Closed stays Closed).
func onData(s ConnState) ConnState {
	switch s {
	case StateResetting, StateQuiet:
		return StateAttached
	default:
		return s
	}
}

// onBanner forces Attached: the firmware setup-console banner is the canonical
// attach signal (console.c:361). The firmware prints it wrapped in CRLF, so it is
// matched against a reader-framed line, never anchored against the raw byte stream.
func onBanner(ConnState) ConnState { return StateAttached }

// onExit classifies a finished interactive run. This Quiet-vs-Sleeping-vs-Closed
// split is the main correctness risk (realities §3/§4):
//   - operatorInitiated (disconnect / port-release / pane-close)        → Closed
//   - execErr (the child never really streamed: start/transport failure) → Error
//   - otherwise a genuine reader EOF / process exit while we expected
//     liveness = the device slept out from under us / the link died      → Sleeping
//
// A still-open link that merely fell silent never reaches here — that is the
// timer-driven onSilence (Quiet), not an exit.
func onExit(operatorInitiated, execErr bool) ConnState {
	switch {
	case operatorInitiated:
		return StateClosed
	case execErr:
		return StateError
	default:
		return StateSleeping
	}
}
