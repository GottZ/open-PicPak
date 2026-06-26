package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/layout"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// The global message taxonomy (wm-shell §4.3). Background goroutines and panes
// speak only this vocabulary to the root; the root routes accordingly.
//
// There is intentionally NO BackgroundTickMsg: per-pane cadence is each pane's own
// tea.Tick (K3). The shell never broadcasts a tick.

// PaneMsg is the addressed envelope: a payload destined for exactly one pane,
// delivered whether that pane is focused or backgrounded. This is the ONLY
// background-delivery path (K2). Background goroutines emit it via their Sender;
// the root also re-addresses a pane-returned Cmd's eventual Msg back into a
// PaneMsg{To: id} so panes never see global addressing.
type PaneMsg struct {
	To      pane.PaneID
	Payload tea.Msg
}

// SpawnPaneMsg asks the root to construct a pane of Kind via its registered
// factory, seed it from Args, and (optionally) focus it.
type SpawnPaneMsg struct {
	Kind  pane.PaneKind
	Args  map[string]string
	Focus bool
}

// ClosePaneMsg asks the root to tear down one pane (cancel its ctx, Close()).
type ClosePaneMsg struct {
	ID pane.PaneID
}

// FocusPaneMsg moves focus to a pane (cosmetic; opens/closes no transport, R5).
type FocusPaneMsg struct {
	ID pane.PaneID
}

// LayoutMsg requests a layout operation (next/prev tab, future splits).
type LayoutMsg struct {
	Op layout.Op
}

// StatusMsg sets the shell status line text.
type StatusMsg string

// FatalMsg is a pane reporting an unrecoverable error. The app survives: the
// pane is marked errored and the shell surfaces the message; it does not quit.
type FatalMsg struct {
	ID  pane.PaneID
	Err error
}

// QuitMsg requests application teardown + exit (distinct from a raw key: a pane
// or command can request a clean quit through the same guarded path).
type QuitMsg struct{}

// ConfigChangedMsg (K4) is emitted to every pane (as an addressed PaneMsg) after
// the shell swaps its *config.Config on a successful reload, so a pane can re-read
// any keys it cares about. The shell holds the new pointer; the pane reads it.
type ConfigChangedMsg struct {
	Config *config.Config
}

// ReleaseDevicePortMsg (K6) asks whoever holds a device port to release it before
// another axis (flash) claims the VID-gated port. W3 only routes it; the console
// pane honors it in W6 and flash emits it in W7.
type ReleaseDevicePortMsg struct {
	Port string
}

// SpawnArgsMsg seeds a freshly spawned pane with its SpawnPaneMsg.Args. The root
// delivers it as an addressed PaneMsg right after Init so the factory signature
// stays arg-free (see registry.go). A pane that needs args handles this in Update;
// one that does not ignores it.
type SpawnArgsMsg struct {
	Args map[string]string
}
