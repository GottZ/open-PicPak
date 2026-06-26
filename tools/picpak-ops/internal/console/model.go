package console

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// Init opens nothing: connect = reset, so the pane stays idle until an explicit
// focused connect keystroke (or, with auto_connect, until the spawn args arrive). A
// layout-restored pane therefore never reboots a device on creation.
func (p *consolePane) Init() tea.Cmd { return nil }

// Update folds addressed messages into the pane. Addressed payloads (spawn args,
// device bytes, exit, config change, port release) are always delivered — focused or
// backgrounded (K2) — so the scrollback stays warm and the session survives unfocus.
// Key messages arrive only while focused (the shell gates them), so typing in another
// pane never leaks into the device. It never blocks and runs no subprocess inline.
func (p *consolePane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {

	case app.SpawnArgsMsg:
		return p, p.handleSpawnArgs(m.Args)

	case app.ConfigChangedMsg:
		if m.Config != nil {
			p.cfg = m.Config
			p.cc = m.Config.Console
			p.colors = resolveColors(m.Config)
			p.keys = newKeymap(m.Config.Console.Keys)
			if re, err := regexp.Compile(m.Config.Console.BannerMatch); err == nil {
				p.bannerRe = re
			}
		}
		return p, nil

	case app.ReleaseDevicePortMsg: // K6: release the VID-gated port for flash
		if p.session != nil && m.Port != "" && m.Port == p.session.Port() && p.session.Running() {
			p.session.Stop() // operator-class teardown; remote trap reaps the cat
			p.state = StateClosed
			p.sb.Append("device port released for another axis (flash) — session closed.")
			p.markDirty()
		}
		return p, nil

	// --- live transport (the single sshhost run, K9) ---------------------------
	case sshhost.RunOutputMsg:
		p.applyData(frame(m.Line))
		return p, nil
	case sshhost.RunExitMsg:
		return p, p.applyExit(m.Code, m.Err)

	// --- console vocabulary (also the test seams) ------------------------------
	case ConsoleDataMsg:
		p.applyData(m.Lines)
		return p, nil
	case ConsoleBannerMsg:
		p.state = onBanner(p.state)
		return p, nil
	case ConsoleStateMsg:
		p.state = m.State
		if m.Err != nil {
			p.lastErr = m.Err
			p.sb.Append("state: " + m.Err.Error())
			p.markDirty()
		}
		return p, nil
	case ConsoleErrMsg:
		p.lastErr = m.Err
		p.state = StateError
		if m.Err != nil {
			p.sb.Append("error: " + m.Err.Error())
		}
		p.markDirty()
		return p, nil
	case ConsoleExitMsg:
		return p, p.applyExit(m.Code, m.Err)

	// --- pane-own cadence ------------------------------------------------------
	case quietTickMsg:
		return p, p.onQuietTick()
	case reconnectMsg:
		return p, p.onReconnect()

	case tea.KeyPressMsg: // focused only (shell gates keys)
		return p, p.onKey(m)
	}
	return p, nil
}

// handleSpawnArgs seeds the device the console attaches to and resolves its transport
// (the single sshhost Runner, K9). With auto_connect it connects immediately (after
// the args, since they arrive post-Init); otherwise it waits for the focused connect
// key. A host that is neither local nor a [[hosts]] entry is recorded as connErr and
// the pane refuses to connect, surfacing the reason.
func (p *consolePane) handleSpawnArgs(args map[string]string) tea.Cmd {
	host := args["host"]
	port := args["port"]
	if port == "" {
		port = args["tty"]
	}
	p.dev = deviceRef{
		Host:    host,
		Port:    port,
		Serial:  args["serial"],
		Label:   args["label"],
		IsLocal: isLocalHost(p.cfg, host),
	}
	p.hadArgs = true

	runner, err := sshhost.RunnerFor(p.cfg, host)
	if err != nil {
		p.connErr = err
		p.state = StateError
		p.sb.Append("console: cannot resolve host: " + err.Error())
		p.markDirty()
		return nil
	}
	p.connErr = nil
	p.session = newSession(p.Context(), runner, p.cc, p.dev, p.ID(), p.Sender())

	if p.cc.AutoConnect {
		return p.connect(true) // explicit operator opt-in bypasses the confirm prompt
	}
	return nil
}

// connect performs the explicit connect = reset + attach. confirmed reports whether
// the confirm_connect guard is satisfied. It refuses without a resolved gated port
// (never attaches to a port discovery did not mark — protects the Zigbee dongle) and
// starts the single sshhost transport. Returns the quiet-clock tick once attached.
func (p *consolePane) connect(confirmed bool) tea.Cmd {
	if p.connErr != nil || p.session == nil {
		p.sb.Append("console: not ready to connect (no resolved host/device).")
		p.markDirty()
		return nil
	}
	if p.dev.Port == "" {
		p.sb.Append("console: no gated device port — refusing to attach.")
		p.state = StateError
		p.markDirty()
		return nil
	}
	if p.session.Running() {
		return nil
	}
	if p.cc.ConfirmConnect && !confirmed {
		p.confirmArmed = true
		p.sb.Append("connect RESETS the device — press the connect key again to confirm.")
		p.markDirty()
		return nil
	}
	p.confirmArmed = false
	if err := p.session.Start(); err != nil {
		p.lastErr = err
		p.state = StateError
		p.sb.Append("connect failed: " + err.Error())
		p.markDirty()
		return nil
	}
	p.state = StateResetting
	p.lastActivity = time.Now().UnixNano()
	p.sb.Append("resetting + attaching… (DTR/RTS pulse → device reboots)")
	p.refreshViewport()
	p.markDirty()
	return quietTickCmd(p.quietTimeout())
}

// applyData appends framed device lines — always, focused or background (K2) — checks
// the banner per framed line (Resetting/Quiet → Attached), and refreshes the quiet
// clock so still-flowing output stays Attached.
func (p *consolePane) applyData(lines []string) {
	if len(lines) == 0 {
		return
	}
	for _, ln := range lines {
		p.sb.Append(ln)
		if p.matchBanner(ln) {
			p.state = onBanner(p.state)
		} else {
			p.state = onData(p.state)
		}
	}
	p.lastActivity = time.Now().UnixNano()
	p.refreshViewport()
	p.markDirty()
}

// onQuietTick is the quiet heuristic clock: while a session is live and the link has
// been silent for quiet_timeout_ms on a STILL-OPEN stream, Attached flips to Quiet
// (log-only, not a drop). It re-arms only while the session runs.
func (p *consolePane) onQuietTick() tea.Cmd {
	if p.session == nil || !p.session.Running() {
		return nil
	}
	d := p.quietTimeout()
	if p.state == StateAttached && time.Duration(time.Now().UnixNano()-p.lastActivity) >= d {
		p.state = onSilence(p.state) // Attached → Quiet
		p.markDirty()
	}
	return quietTickCmd(d)
}

// applyExit classifies a finished interactive run (the Quiet-vs-Sleeping-vs-Closed
// split, realities §3/§4): operator/port-release/close → Closed; an exec/start
// failure → Error; a genuine EOF while we expected liveness → Sleeping. A transient
// Sleeping drop may auto-reconnect (bounded by reconnect_max_attempts) — which itself
// resets the device, hence the bound.
func (p *consolePane) applyExit(code int, err error) tea.Cmd {
	operator := p.session != nil && p.session.Closing()
	execErr := err != nil && !operator
	if p.session != nil {
		p.session.finish() // tear down goroutine/child; keeps the Closing flag intact
	}
	p.state = onExit(operator, execErr)
	p.markDirty()

	switch p.state {
	case StateClosed:
		p.reconnects = 0
		p.sb.Append("session closed.")
		p.refreshViewport()
		return nil
	case StateError:
		p.lastErr = err
		p.sb.Append(fmt.Sprintf("session error (code=%d): %v", code, err))
		p.refreshViewport()
		return nil
	default: // Sleeping
		p.sb.Append("link dropped (device slept / keep-awake lost).")
		if p.cc.Reconnect && p.reconnects < p.cc.ReconnectMaxAttempts {
			p.reconnects++
			p.state = StateReconnecting
			backoff := time.Duration(p.cc.ReconnectBackoffMS) * time.Millisecond
			p.sb.Append(fmt.Sprintf("auto-reconnect %d/%d in %s (resets the device)…", p.reconnects, p.cc.ReconnectMaxAttempts, backoff))
			p.refreshViewport()
			return reconnectCmd(backoff)
		}
		p.refreshViewport()
		return nil
	}
}

// onReconnect restarts a dropped session after the backoff (a fresh reset + attach).
func (p *consolePane) onReconnect() tea.Cmd {
	if p.session == nil || p.session.Running() {
		return nil
	}
	if err := p.session.Start(); err != nil {
		p.lastErr = err
		p.state = StateError
		p.sb.Append("reconnect failed: " + err.Error())
		p.markDirty()
		return nil
	}
	p.state = StateResetting
	p.lastActivity = time.Now().UnixNano()
	return quietTickCmd(p.quietTimeout())
}

// onKey handles pane-local bindings while focused (global chrome keys never reach
// here, K7). An unbound key falls through to the text input editor.
func (p *consolePane) onKey(k tea.KeyPressMsg) tea.Cmd {
	switch p.keys.resolve(k.String()) {
	case actSubmit:
		return p.submit()
	case actHistoryPrev:
		p.historyPrev()
		return nil
	case actHistoryNext:
		p.historyNext()
		return nil
	case actScrollUp:
		p.followTail = false
		p.vp.ScrollUp(3)
		return nil
	case actScrollDown:
		p.vp.ScrollDown(3)
		if p.vp.AtBottom() {
			p.followTail = true
		}
		return nil
	case actClear:
		p.sb.Clear()
		p.followTail = true
		p.refreshViewport()
		return nil
	case actReconnect:
		confirmed := !p.cc.ConfirmConnect || p.confirmArmed
		return p.connect(confirmed)
	case actDisconnect:
		if p.session != nil && p.session.Running() {
			p.session.Stop()
			p.state = StateClosed
			p.reconnects = 0
			p.sb.Append("disconnected.")
			p.refreshViewport()
			p.markDirty()
		}
		return nil
	case actHelp:
		p.helpVisible = !p.helpVisible
		return nil
	case actSearch:
		// Reserved: incremental scrollback search is a later refinement; the key is
		// bound (config) so the binding is honored without a no-op leaking to input.
		return nil
	default:
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(k)
		return cmd
	}
}

// submit sends the current input line to the device (line + configured ending), pushes
// history, and — only when local_echo is on — echoes it locally (the firmware echoes,
// so the default avoids doubled characters).
func (p *consolePane) submit() tea.Cmd {
	line := p.input.Value()
	p.input.Reset()
	if line != "" {
		p.history = append(p.history, line)
	}
	p.histPos = len(p.history)

	if p.session != nil && p.session.Running() {
		p.session.SendLine(p.encodeLine(line))
		if p.cc.LocalEcho {
			p.sb.Append("> " + line)
			p.refreshViewport()
		}
		return nil
	}
	p.sb.Append("(not connected — press the connect key to reset + attach)")
	p.refreshViewport()
	p.markDirty()
	return nil
}

// encodeLine appends the configured line ending (firmware reads CR/LF, console.c:65).
func (p *consolePane) encodeLine(line string) []byte {
	switch p.cc.LineEndings {
	case "lf":
		return []byte(line + "\n")
	case "cr":
		return []byte(line + "\r")
	default: // crlf
		return []byte(line + "\r\n")
	}
}

func (p *consolePane) historyPrev() {
	if len(p.history) == 0 {
		return
	}
	if p.histPos > 0 {
		p.histPos--
	}
	p.input.SetValue(p.history[p.histPos])
	p.input.CursorEnd()
}

func (p *consolePane) historyNext() {
	if len(p.history) == 0 {
		return
	}
	if p.histPos < len(p.history) {
		p.histPos++
	}
	if p.histPos >= len(p.history) {
		p.histPos = len(p.history)
		p.input.SetValue("")
		return
	}
	p.input.SetValue(p.history[p.histPos])
	p.input.CursorEnd()
}

// SetFocused clears the unread marker and focuses the input on focus; on blur it stops
// the input and disarms a half-typed confirm (a confirm must not survive losing focus,
// so a backgrounded pane cannot complete a device-resetting connect). Cosmetic only —
// never opens or closes the transport (R5).
func (p *consolePane) SetFocused(focused bool) {
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
		p.input.Focus()
		return
	}
	p.input.Blur()
	p.confirmArmed = false
}

// SetSize recomputes the viewport + input geometry (title + input + footer reserve 3
// rows) and repaints the scrollback.
func (p *consolePane) SetSize(width, height int) {
	p.BasePane.SetSize(width, height)
	w, h := width, height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	vpH := h - 3
	if vpH < 1 {
		vpH = 1
	}
	p.vp.SetWidth(w)
	p.vp.SetHeight(vpH)
	p.input.SetWidth(w - 2)
	p.refreshViewport()
}

// Meta gives the pane its live title (host/device/state) and an honest status:
// Working while connecting/reconnecting, Active while attached, Quiet on a silent live
// link, Error on a dead/failed link, else Idle. Dirty drives the unread marker.
func (p *consolePane) Meta() pane.PaneMeta {
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: statusFor(p.state),
		Dirty:  p.dirty,
	}
}

// Close cancels the session (operator-class teardown: kills the ssh child; the remote
// trap reaps the remote cat) and releases the pane. Idempotent.
func (p *consolePane) Close() error {
	if p.session != nil {
		p.session.Stop()
	}
	return nil
}

// markDirty flags unseen output when the pane is not focused (the unread marker).
func (p *consolePane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}

// refreshViewport repaints the scrollback ring into the viewport, following the tail
// only while pinned (a manual scroll-up unpins; scrolling back to the bottom re-pins).
func (p *consolePane) refreshViewport() {
	p.vp.SetContentLines(p.sb.Lines())
	if p.followTail {
		p.vp.GotoBottom()
	}
}

// quietTimeout is the configured quiet_timeout_ms as a Duration (sane fallback on a
// non-positive value).
func (p *consolePane) quietTimeout() time.Duration {
	d := time.Duration(p.cc.QuietTimeoutMS) * time.Millisecond
	if d <= 0 {
		d = 2 * time.Second
	}
	return d
}

// matchBanner reports whether a framed line is the firmware setup-console banner. It
// matches against a single reader-framed line (the firmware wraps the banner in CRLF),
// never anchored against the raw byte stream. A bad/empty pattern falls back to a
// substring match.
func (p *consolePane) matchBanner(line string) bool {
	if p.cc.BannerMatch == "" {
		return false
	}
	if p.bannerRe != nil {
		return p.bannerRe.MatchString(line)
	}
	return strings.Contains(line, p.cc.BannerMatch)
}

// statusFor maps the link state to the chrome status glyph role.
func statusFor(s ConnState) pane.PaneStatus {
	switch s {
	case StateResetting, StateReconnecting:
		return pane.StatusWorking
	case StateAttached:
		return pane.StatusActive
	case StateQuiet:
		return pane.StatusQuiet
	case StateSleeping, StateError:
		return pane.StatusError
	default:
		return pane.StatusIdle
	}
}

// title renders "console · <host> · <device> · <state>"; host/device are runtime
// device-record values (never shipped literals), state is the live link state.
func (p *consolePane) title() string {
	host := p.dev.Host
	if host == "" {
		host = "—"
	}
	dev := p.dev.Label
	if dev == "" {
		dev = p.dev.Serial
	}
	if dev == "" {
		dev = p.dev.Port
	}
	if dev == "" {
		dev = "(no device)"
	}
	return fmt.Sprintf("console · %s · %s · %s", host, dev, p.state.String())
}

// isLocalHost reports whether a host runs via the local runner (os/exec): the "local"
// sentinel / empty, or a [[hosts]] entry with local=true. The local pump needs a shell
// wrap (transport.go); an ssh host sends the verbatim command to its remote shell.
func isLocalHost(cfg *config.Config, host string) bool {
	if host == "" || host == "local" {
		return true
	}
	if h, ok := cfg.HostByName(host); ok {
		return h.Local
	}
	return false
}
