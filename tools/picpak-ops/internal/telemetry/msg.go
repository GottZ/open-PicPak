package telemetry

import "time"

// The pane's private message taxonomy. A poll runs inside a tea.Cmd goroutine and
// returns one of these; the wm-shell router re-addresses a pane-returned Cmd's Msg
// back to this pane as app.PaneMsg{To:id} (K2), so the goroutine never touches pane
// state and never fans out. Each msg carries its own timestamp so the view can show
// the two freshnesses (poll-time vs device-report-time) honestly.

// tickMsg is the pane's OWN cadence tick (K3): telemetry runs a private tea.Tick at a
// fixed base granularity and polls when its focused/background interval has elapsed,
// rather than depending on a removed shell BackgroundTick.
type tickMsg struct{}

// fleetMsg carries a completed fleet snapshot (read-shape 1).
type fleetMsg struct {
	rows []FleetRow
	at   time.Time
}

// deviceMsg carries a completed per-device detail + history fetch (read-shape 2).
type deviceMsg struct {
	serial  string
	latest  *DeviceTelemetry // nil when the device has no telemetry yet
	history []HistoryPoint
	at      time.Time
}

// pollErrMsg reports a failed poll. It is NON-FATAL: the pane keeps its last good data
// and keeps ticking, surfacing a stale/error badge (design 08 §4.1).
type pollErrMsg struct {
	err error
	at  time.Time
}
