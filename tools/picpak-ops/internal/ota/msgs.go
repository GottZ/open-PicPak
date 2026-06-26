package ota

import "time"

// The pane's private message taxonomy. A read/write runs inside a tea.Cmd goroutine and
// returns one of these; the wm-shell router re-addresses a pane-returned Cmd's Msg back
// to this pane as app.PaneMsg{To:id} (K2), so the goroutine never touches pane state and
// never fans out — a write started while focused still delivers its result after the
// operator switches away.

// tickMsg is the pane's OWN cadence tick (K3): the OTA pane runs a private tea.Tick at a
// fixed base granularity and reloads when its focused/background interval has elapsed,
// rather than depending on a removed shell BackgroundTick.
type tickMsg struct{}

// otaStateMsg carries a completed read snapshot (versions + channels + rollouts +
// assembled per-device target-vs-running).
type otaStateMsg struct {
	state OTAState
	at    time.Time
}

// fwArtifactScannedMsg carries a completed firmware artifact scan (version + lowercase
// sha256 + size), used to prefill the register form.
type fwArtifactScannedMsg struct {
	art FirmwareArtifact
	err error
}

// writeResultMsg reports a completed write. kind names the operation (for the status
// line); err is nil on success. A successful write sets the pending-write badge and
// triggers a refresh.
type writeResultMsg struct {
	kind string
	err  error
}

// otaErrMsg reports a failed read. It is NON-FATAL: the pane keeps its last good state
// and keeps ticking, surfacing a stale/error badge.
type otaErrMsg struct {
	err error
	at  time.Time
}
