package console

// The console axis message vocabulary. Every type here travels as the Payload of a
// wm-shell app.PaneMsg{To: <this pane>} (K2 addressed delivery): a background
// goroutine / session never speaks to the Model directly — it calls the injected
// Sender with an addressed PaneMsg and the root router delivers it to this pane
// whether it is focused or backgrounded, so the scrollback stays warm while unfocused.
//
// The live device-byte and exit paths arrive as sshhost.RunOutputMsg /
// sshhost.RunExitMsg (the single transport, K9) addressed to this pane; the model
// funnels those through the same handlers these types drive, so a test can exercise
// the data/exit/state logic without a real transport.

// ConsoleDataMsg carries already-framed device lines ready to append to the
// scrollback (the live path frames a sshhost.RunOutputMsg into one).
type ConsoleDataMsg struct {
	Lines []string
}

// ConsoleBannerMsg signals the firmware setup-console banner was seen on a framed
// line — the canonical "attached" signal (Resetting → Attached).
type ConsoleBannerMsg struct{}

// ConsoleStateMsg is a session-driven connection-state transition (e.g. a scheduled
// reconnect flipping to Reconnecting). Err, when set, is surfaced as a system line.
type ConsoleStateMsg struct {
	State ConnState
	Err   error
}

// ConsoleErrMsg is a non-fatal error to surface as a styled system line (a
// transport-build / start failure that produced no sshhost run).
type ConsoleErrMsg struct {
	Err error
}

// ConsoleExitMsg mirrors a finished interactive run into the console's vocabulary so
// the model classifies the exit (operator-close → Closed, EOF/keep-awake-loss →
// Sleeping, exec failure → Error). The live path uses sshhost.RunExitMsg directly.
type ConsoleExitMsg struct {
	Code int
	Err  error
}
