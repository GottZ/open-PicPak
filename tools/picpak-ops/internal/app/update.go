package app

import (
	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/keys"
	"github.com/open-picpak/picpak-ops/internal/layout"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// Update is the root dispatch (wm-shell §4.3). First match wins, in this order:
//
//  1. WindowSizeMsg → resize layout + every pane.
//  2. Global chrome keys (resolved by the keymap + chord machine) → handled here.
//  3. FocusMsg/BlurMsg → cosmetic pause/resume (no transport).
//  4. PaneMsg{To} → routed to the one addressed pane (FG or BG); dead id dropped.
//  5. Spawn/Close/Focus/Layout/Status/Fatal/Quit/ConfigChanged/ReleasePort → ops.
//  6. Remaining key/mouse → focused pane only (mouse via the layout hit-test).
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg: // 1
		return m, m.onResize(msg.Width, msg.Height)

	case tea.KeyPressMsg: // 2 (+ falls through to 6 when not a global action)
		return m.onKey(msg)

	case tea.FocusMsg: // 3
		return m, nil // cosmetic only; panes self-pace, nothing to forward
	case tea.BlurMsg: // 3
		return m, nil

	case PaneMsg: // 4
		return m, m.routePaneMsg(msg.To, msg.Payload)

	case config.ReloadMsg: // 5 (K4 hot-reload, re-arm the watch)
		return m, m.onReload(msg)

	case SpawnPaneMsg: // 5
		return m, m.spawn(msg.Kind, msg.Args, msg.Focus)
	case ClosePaneMsg: // 5
		return m, m.closePane(msg.ID)
	case FocusPaneMsg: // 5
		m.focusPane(msg.ID)
		return m, nil
	case LayoutMsg: // 5
		m.onLayoutOp(msg.Op)
		return m, nil
	case StatusMsg: // 5
		m.status = string(msg)
		return m, nil
	case FatalMsg: // 5 — app survives; surface the error, mark the pane
		m.fatal = msg.Err
		return m, nil
	case QuitMsg: // 5
		return m, m.beginQuit()
	case ReleaseDevicePortMsg: // 5 (K6) — broadcast to every pane as addressed msg
		return m, m.broadcast(msg)

	case tea.MouseClickMsg: // 6
		return m, m.onMouse(msg)
	case tea.MouseWheelMsg: // 6
		return m, m.onMouse(msg)
	case tea.MouseMotionMsg: // 6
		return m, m.onMouse(msg)
	}

	return m, nil
}

// onResize stores the size, recomputes the layout body size, and SetSize's every
// pane (FG and BG so a backgrounded pane is correctly sized when refocused).
func (m *Model) onResize(w, h int) tea.Cmd {
	m.width, m.height = w, h
	bodyW, bodyH := m.layout.SetSize(w, h)
	for _, p := range m.reg.all() {
		p.SetSize(bodyW, bodyH)
	}
	return nil
}

// onKey resolves a key through the global keymap + chord machine. A global action
// is handled here and never forwarded; a swallowed chord prefix updates the status
// hint; anything else is passed to the focused pane (§4.3 step 6).
func (m *Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	res := m.keys.Resolve(k)
	// Any key that is not the quit-confirm disarms a pending quit guard.
	switch res.Verdict {
	case keys.Swallowed:
		// A chord prefix is armed; surface it and consume the key.
		return m, nil
	case keys.GlobalAction:
		if res.Action != "quit" {
			m.quitArmed = false
		}
		return m, m.runAction(res.Action)
	default: // keys.PassToPane
		m.quitArmed = false
		return m, m.forwardToFocused(k)
	}
}

// runAction executes a resolved global chrome action.
func (m *Model) runAction(action string) tea.Cmd {
	switch action {
	case "quit":
		return m.beginQuit()
	case "help":
		m.help.visible = !m.help.visible
		return nil
	case "next_tab":
		m.layout.Tabs().Next()
		m.applyFocus()
		return nil
	case "prev_tab":
		m.layout.Tabs().Prev()
		m.applyFocus()
		return nil
	case "close_pane":
		if id, ok := m.layout.Tabs().Focused(); ok {
			return m.closePane(id)
		}
		return nil
	case "split":
		// Tabs-first (OQ#2): split is reserved; no-op until the split refinement.
		return nil
	}
	// spawn.<kind> dynamic actions.
	if kind, ok := spawnKind(action); ok {
		return m.spawn(kind, nil, true)
	}
	return nil
}

// spawnKind extracts the pane kind from a "spawn.<kind>" action name.
func spawnKind(action string) (pane.PaneKind, bool) {
	const prefix = "spawn."
	if len(action) > len(prefix) && action[:len(prefix)] == prefix {
		return pane.PaneKind(action[len(prefix):]), true
	}
	return "", false
}

// forwardToFocused routes a key to the focused pane only (§4.3 step 6). The
// pane-returned Cmd is re-addressed so its Msg comes back as an addressed PaneMsg.
func (m *Model) forwardToFocused(msg tea.Msg) tea.Cmd {
	id, ok := m.layout.Tabs().Focused()
	if !ok {
		return nil
	}
	return m.routePaneMsg(id, msg)
}

// onMouse hit-tests the click and routes it: a tab click switches tab; a body
// click focuses the pane and is forwarded with pane-local coordinates (the only
// mouse path — never View.OnMouse, §header). Wheel/motion in the body forward to
// the focused pane too.
func (m *Model) onMouse(msg tea.Msg) tea.Cmd {
	mm, ok := msg.(tea.MouseMsg)
	if !ok {
		return nil
	}
	m.layout.Refresh(m.reg.all()) // align tab spans with live meta before hit-test
	mouse := mm.Mouse()
	hit := m.layout.HitTest(mouse.X, mouse.Y)
	switch hit.Zone {
	case layout.ZoneTab:
		m.focusPane(hit.ID)
		return nil
	case layout.ZoneBody:
		// Focus the pane the click landed in, then forward the raw mouse msg to it.
		// (Pane-local translation is exposed via HitTest for panes that need it;
		// the raw msg is forwarded so the pane can re-hit-test if it wants.)
		m.focusPane(hit.ID)
		return m.routePaneMsg(hit.ID, msg)
	default:
		return nil
	}
}

// onLayoutOp applies a tab/layout operation.
func (m *Model) onLayoutOp(op layout.Op) {
	switch op {
	case layout.OpNextTab:
		m.layout.Tabs().Next()
		m.applyFocus()
	case layout.OpPrevTab:
		m.layout.Tabs().Prev()
		m.applyFocus()
	case layout.OpSplit:
		// reserved (tabs-first)
	}
}

// spawn constructs a pane, runs Init, registers a tab, applies focus, and seeds
// the pane with its args. It returns a batched Cmd (Init follow-up + arg seed).
func (m *Model) spawn(kind pane.PaneKind, args map[string]string, focus bool) tea.Cmd {
	p, id, err := m.reg.spawn(kind)
	if err != nil {
		m.status = err.Error()
		return nil
	}
	bodyW, bodyH := m.layout.BodySize()
	p.SetSize(bodyW, bodyH)
	m.layout.Tabs().Add(id, focus)
	m.applyFocus()

	cmds := []tea.Cmd{m.initPane(id, p)}
	if len(args) > 0 {
		// Deliver args as an addressed PaneMsg so the factory stays arg-free.
		cmds = append(cmds, func() tea.Msg {
			return PaneMsg{To: id, Payload: SpawnArgsMsg{Args: args}}
		})
	}
	return tea.Batch(cmds...)
}

// closePane tears down a pane: registry.remove cancels its ctx and Close()s it,
// then the tab is dropped and focus re-applied. If it was the last pane, drop to a
// fresh launcher (OQ#3 default: closing the last pane lands on the launcher).
func (m *Model) closePane(id pane.PaneID) tea.Cmd {
	if _, ok := m.reg.lookup(id); !ok {
		return nil
	}
	_ = m.reg.remove(id)
	m.layout.Tabs().Remove(id)
	if m.reg.len() == 0 {
		cmd := m.spawn(pane.KindLauncher, nil, true)
		m.applyFocus()
		return cmd
	}
	m.applyFocus()
	return nil
}

// focusPane sets focus to a pane id (cosmetic; no transport — R5).
func (m *Model) focusPane(id pane.PaneID) {
	if m.layout.Tabs().Focus(id) {
		m.applyFocus()
	}
}

// applyFocus syncs each pane's SetFocused flag with the layout's focus pointer.
// Exactly one pane is focused; the rest are blurred (they keep running). A focus
// change clears any pending chord prefix so a half-typed chord does not leak.
func (m *Model) applyFocus() {
	focused, ok := m.layout.Tabs().Focused()
	for id, p := range m.reg.all() {
		p.SetFocused(ok && id == focused)
	}
	m.keys.Reset()
}

// broadcast delivers a control message to every live pane as an addressed PaneMsg
// (K6 ReleaseDevicePortMsg + ConfigChangedMsg use this). It returns a Batch of the
// re-addressed follow-up Cmds.
func (m *Model) broadcast(payload tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range m.reg.orderIDs() {
		if cmd := m.routePaneMsg(id, payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// onReload handles a config hot-reload (K4). On success it swaps the *Config
// pointer everywhere (model, registry, layout theme/opts, keymap), then notifies
// every pane via an addressed ConfigChangedMsg; on error it keeps the last-good
// config and surfaces a non-fatal toast. It always re-arms the one-shot watch Cmd.
func (m *Model) onReload(msg config.ReloadMsg) tea.Cmd {
	var cmds []tea.Cmd
	if msg.Err == nil && msg.Config != nil {
		m.cfg = msg.Config
		m.reg.setConfig(msg.Config)
		m.keys = keys.New(mustKeymap(msg.Config))
		m.layout.SetTheme(resolveTheme(msg.Config))
		m.layout.SetOptions(resolveLayoutOpts(msg.Config))
		// Re-apply size so the new chrome options recompute the body size.
		m.layout.SetSize(m.width, m.height)
		if c := m.broadcast(ConfigChangedMsg{Config: msg.Config}); c != nil {
			cmds = append(cmds, c)
		}
		m.status = "" // clear any prior reload-error toast
	} else if msg.Err != nil {
		m.status = "config reload failed (keeping last good): " + msg.Err.Error()
	}

	// Re-arm the one-shot watch.
	if m.reloadPath != "" && m.cfg.Reload.Watch {
		opts := m.reloadOpts
		opts.ReloadDebounce = m.cfg.Reload.Debounce.D()
		if wc := config.WatchCmd(m.rootCtx, m.reloadPath, opts); wc != nil {
			cmds = append(cmds, wc)
		}
	}
	return tea.Batch(cmds...)
}

// beginQuit implements the quit-guard (K5): if any pane reports StatusWorking, the
// first quit arms a confirm (no quit); the second quit confirms and tears down.
// With no Working pane it quits immediately.
func (m *Model) beginQuit() tea.Cmd {
	if m.anyWorking() && !m.quitArmed {
		m.quitArmed = true
		m.status = m.quitConfirmPrompt()
		return nil
	}
	m.quitting = true
	m.teardown()
	return tea.Quit
}

// anyWorking reports whether any pane's Meta().Status is StatusWorking (a unit of
// work in flight — build/flash/poll). This is the seam build/flash rely on.
func (m *Model) anyWorking() bool {
	for _, p := range m.reg.all() {
		if p.Meta().Status == pane.StatusWorking {
			return true
		}
	}
	return false
}
