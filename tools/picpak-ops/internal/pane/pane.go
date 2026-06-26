// Package pane defines the canonical pane contract every feature axis implements.
//
// This is the single source of truth for the Pane interface, the pane identity
// and chrome types, and the Sender bridge a pane hands to its background
// goroutines. It is deliberately pin-light: it depends only on the Bubble Tea
// message/command types so every other package can import it without pulling in
// transport (ssh, pgx, exec) dependencies.
//
// A pane is a composed string-renderer, NOT a tea.Model: the wm-shell root is
// the only tea.Model, and it composes each pane's View() string into its own
// tea.View. Panes therefore return a plain string from View; size arrives once
// per relayout via SetSize (not as View arguments).
package pane

import tea "charm.land/bubbletea/v2"

// PaneID is a stable, unique pane identifier. Its value is config-derived at
// runtime (e.g. "console:<host>:<port>"), never a code literal.
type PaneID string

// PaneKind names the feature kind a pane implements; used by the registry to
// pick a factory and by the chrome to label tabs.
type PaneKind string

const (
	KindLauncher  PaneKind = "launcher"
	KindHosts     PaneKind = "hosts"
	KindBuild     PaneKind = "build"
	KindConsole   PaneKind = "console"
	KindFlash     PaneKind = "flash"
	KindOTA       PaneKind = "ota"
	KindTelemetry PaneKind = "telemetry"
	KindLogs      PaneKind = "logs"
)

// Sender is the injected bridge a pane hands to its background goroutines. It is
// a thin, goroutine-safe wrapper over (*tea.Program).Send, set once at startup.
// A goroutine reaches its pane by calling send with an addressed message; the
// root router delivers it whether the pane is focused or backgrounded.
type Sender func(tea.Msg)

// PaneStatus is a small, pane-supplied enum so the tab bar is honest about
// sleeping/sparse devices instead of a misleading binary "live". A poll-only
// telemetry pane is Working only while a poll is in flight; a tethered-but-quiet
// console is Quiet (link silent), not "active".
type PaneStatus int

const (
	StatusIdle    PaneStatus = iota // no background goroutine running
	StatusActive                    // background goroutine alive AND data flowing
	StatusWorking                   // background goroutine alive, in a unit of work (poll/flash/build)
	StatusQuiet                     // background goroutine alive but source currently silent
	StatusError                     // pane reported a non-fatal error; still alive
)

// String renders the status for diagnostics; the glyph/colour mapping lives in
// the layout chrome (config theme roles), not here.
func (s PaneStatus) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusActive:
		return "active"
	case StatusWorking:
		return "working"
	case StatusQuiet:
		return "quiet"
	case StatusError:
		return "error"
	default:
		return "unknown"
	}
}

// PaneSpec describes a pane to spawn (from ui.startup_panes or a spawn binding).
// It is defined here — not in the config package — so config can decode it via a
// type-only import of this package, keeping config the leaf root with no cycle.
// args carry host/port/DSN-bearing values at runtime; any committed example must
// use placeholders only (air-gap).
type PaneSpec struct {
	Kind  PaneKind          `toml:"kind"`
	Args  map[string]string `toml:"args"`
	Focus bool              `toml:"focus"`
}

// PaneMeta is what the layout/tabs need without reaching into pane internals.
type PaneMeta struct {
	ID     PaneID
	Kind   PaneKind
	Title  string     // shown in the tab bar; pane-provided, may change (config-derived, never literal)
	Status PaneStatus // drives the chrome status glyph
	Dirty  bool       // pane has unseen output since last focused (drives the unread marker)
}

// Pane is the canonical cross-axis contract. Every feature pane implements it,
// typically by embedding BasePane and supplying only Init/Update/View/Close.
type Pane interface {
	// Init is called once when the pane is spawned. It may return a Cmd that
	// launches the pane's long-lived background goroutine (bound to its Sender)
	// and/or arms the pane's own tea.Tick poll loop. Idempotent.
	Init() tea.Cmd

	// Update handles messages routed to THIS pane only: addressed payloads
	// (always delivered, focused or background) plus key/mouse only when
	// focused. Must not block. Returns the (possibly mutated) pane and a Cmd.
	Update(msg tea.Msg) (Pane, tea.Cmd)

	// View renders the pane body into the last size set via SetSize. Pure;
	// called only when visible. Returns a plain string the root composes.
	View() string

	// SetFocused tells the pane whether it owns input. A blurred pane KEEPS
	// RUNNING; this is cosmetic/state only and MUST NOT open or close any
	// transport (focus is not I/O).
	SetFocused(focused bool)

	// SetSize is called on spawn and on every relayout.
	SetSize(width, height int)

	// Meta returns current id/kind/title/status/dirty for the chrome.
	Meta() PaneMeta

	// Close cancels work and releases ALL resources (ssh proc, pgx lease,
	// ttyACM fd). Called exactly once by the root during teardown or on close.
	// Idempotent. Returns error (not tea.Cmd): teardown runs outside the
	// Update loop.
	Close() error
}
