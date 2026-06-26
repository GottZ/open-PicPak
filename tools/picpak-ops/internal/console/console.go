// Package console is the device-console pane (axis 06): an interactive, line-oriented
// terminal onto a single PicPak's USB-Serial-JTAG console, surfaced remotely over the
// single sshhost transport (K9). It implements the canonical pane.Pane (embedding
// pane.BasePane) and owns a Session — the interactive sshhost run — whose stdin
// carries operator keystrokes to the remote `cat > {port}` while device bytes arrive
// as addressed sshhost.RunOutputMsg (K2: delivered focused or backgrounded, so the
// scrollback stays warm while the operator works elsewhere).
//
// Reality framing is non-negotiable and surfaced honestly, never hidden:
//   - Connecting pulses DTR/RTS and REBOOTS the device, so connect is always an
//     explicit, confirmed, focused keystroke (auto_connect default off). A pane
//     restored from a saved layout starts unfocused + idle, so layout-restore never
//     resets a device.
//   - After the firmware exits its console it uninstalls the USB driver and output
//     thins to log-only on a STILL-LIVE link that does not re-enumerate → that is
//     Quiet, not a drop. A genuine EOF (keep-awake lost, device slept) → Sleeping.
//     Operator/ssh teardown → Closed. This split is the main correctness risk.
//   - The interactive input window is a ~3 s peek outside setup mode; the pane says so.
//
// console is multi-instance: one pane (one Session) per device; the shell never
// multiplexes them onto one transport.
package console

import (
	"regexp"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// consolePane implements pane.Pane for the console axis. Its fields are mutated only
// on the message loop (Update) except the Session's own goroutine-owned state, so it
// needs no lock of its own.
type consolePane struct {
	pane.BasePane

	cfg      *config.Config // full config (HostByName / RunnerFor on connect)
	cc       config.Console // console section snapshot
	colors   viewColors
	keys     keymap
	catalog  []Command
	bannerRe *regexp.Regexp // compiled banner_match; nil → substring fallback

	session *Session
	dev     deviceRef
	hadArgs bool
	connErr error // host/runner could not be resolved → the pane refuses to connect, shows it

	state ConnState

	sb         *pane.Scrollback
	vp         viewport.Model
	input      textinput.Model
	followTail bool

	history []string
	histPos int // index into history; len(history) == "editing a fresh line"

	confirmArmed bool // confirm_connect: a first connect keystroke armed the confirm
	reconnects   int  // consecutive auto-reconnect count (bounded by reconnect_max_attempts)
	helpVisible  bool
	dirty        bool
	lastErr      error
	lastActivity int64 // unix-nano of the last device byte (quiet heuristic clock)
}

// New is the console-pane factory (registered for pane.KindConsole in main.go). It
// builds an IDLE pane: no transport opens here (connect = reset). The host/port the
// console attaches to arrive after spawn via an addressed app.SpawnArgsMsg.
func New(base pane.BasePane, cfg *config.Config) pane.Pane {
	cc := cfg.Console

	bannerRe, _ := regexp.Compile(cc.BannerMatch) // nil on error → substring fallback

	vp := viewport.New()
	vp.SoftWrap = true

	in := textinput.New()
	in.Prompt = "> "
	in.SetVirtualCursor(true)
	in.SetSuggestions(CommandNames())
	in.ShowSuggestions = true

	p := &consolePane{
		BasePane:   base,
		cfg:        cfg,
		cc:         cc,
		colors:     resolveColors(cfg),
		keys:       newKeymap(cc.Keys),
		catalog:    Catalog(),
		bannerRe:   bannerRe,
		state:      StateIdle,
		sb:         pane.NewScrollback(cc.ScrollbackLines),
		vp:         vp,
		input:      in,
		followTail: true,
		histPos:    0,
	}
	p.sb.Append("console idle — press the connect key to reset + attach the device.")
	p.sb.Append("connect pulses DTR/RTS: the PicPak reboots. Interactive window is ~3 s outside setup mode.")
	return p
}
