package console

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// quietTickMsg drives the quiet heuristic clock while a session is live. It is the
// pane's own tea.Tick cadence (K3: no shell-wide background tick) and re-arms only
// while a session runs.
type quietTickMsg struct{}

// quietTickCmd schedules the next quiet check after d. The interval is the configured
// quiet_timeout_ms — an UNVERIFIED on-device heuristic (see state.onSilence). A
// non-positive d falls back to a sane default rather than spinning.
func quietTickCmd(d time.Duration) tea.Cmd {
	if d <= 0 {
		d = 2 * time.Second
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return quietTickMsg{} })
}

// reconnectMsg fires after the reconnect backoff to restart a dropped session. A
// reconnect resets the device again, so it is bounded by reconnect_max_attempts.
type reconnectMsg struct{}

// reconnectCmd schedules a session restart after the configured backoff.
func reconnectCmd(d time.Duration) tea.Cmd {
	if d < 0 {
		d = 0
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return reconnectMsg{} })
}
