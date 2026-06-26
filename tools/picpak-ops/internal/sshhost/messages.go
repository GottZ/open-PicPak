package sshhost

import "time"

// The tea.Msg types this axis emits. They are carried as the Payload of a wm-shell
// PaneMsg{To: inventoryPane|runPane} (K2): a background goroutine never speaks to
// the Model directly — it calls send(PaneMsg{To, Payload: <one of these>}) and the
// root router delivers to the addressed pane whether it is focused or backgrounded.
//
// These types intentionally import nothing from internal/app: a payload is just a
// tea.Msg, and the app package wraps it. This keeps sshhost free of an app-package
// import cycle (app → sshhost factory, sshhost ↛ app).

// RunID identifies one *Run so a borrowing pane (flash/console) can match output
// to the run it started. Its value is runtime-minted, never a code literal.
type RunID string

// Stream distinguishes a run's stdout from its stderr in RunOutputMsg.
type Stream int

const (
	Stdout Stream = iota
	Stderr
)

// String renders the stream name for diagnostics / log prefixes.
func (s Stream) String() string {
	if s == Stderr {
		return "stderr"
	}
	return "stdout"
}

// HostPollMsg is emitted after each poll of a host (success or failure). The
// inventory pane folds Devices into its host row; Err (when non-nil) is surfaced as
// an actionable hint. Carries the full post-poll HostState so the pane needs no
// separate state lookup.
type HostPollMsg struct {
	Host    string
	Devices []DiscoveredDevice
	Err     error
	State   ConnState
	At      time.Time
}

// HostStateMsg is emitted when a host's connection-state-machine state changes
// (connecting → up → backoff → down) without a new device list, so the pane can
// update the host badge promptly between polls.
type HostStateMsg struct {
	Host  string
	State ConnState
	Err   error
	At    time.Time
}

// RunOutputMsg carries one line of a *Run's output. The borrowing pane appends it
// to its ring; delivery via the Sender means a backgrounded run pane keeps
// accumulating output (§4.2). Lines are already truncated to run.line_max_bytes.
type RunOutputMsg struct {
	RunID  RunID
	Stream Stream
	Line   string
}

// RunExitMsg is emitted once when a *Run's process has exited and both pipe
// readers have drained. Code is the process exit code (or -1 if it could not be
// determined, e.g. killed by signal); Err carries a wait/exec error if any.
type RunExitMsg struct {
	RunID RunID
	Code  int
	Err   error
}
