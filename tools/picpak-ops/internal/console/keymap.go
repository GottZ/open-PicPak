package console

import "github.com/open-picpak/picpak-ops/internal/config"

// action is a pane-local key action (console.keys.*). It is matched only while the
// pane is focused; global chrome keys (quit/tab/close/spawn) are consumed by the
// shell first (K7 per-pane scope). Anything not bound here falls through to the text
// input editor.
type action int

const (
	actNone action = iota
	actSubmit
	actHistoryPrev
	actHistoryNext
	actScrollUp
	actScrollDown
	actSearch
	actClear
	actReconnect
	actDisconnect
	actHelp
)

// keymap resolves a pressed key string to a pane-local action using the config-owned
// console.keys.* bindings (Policy = Data — no key literal in code). A key bound to
// more than one action keeps the first registered binding; the config validator owns
// double-bind detection.
type keymap struct {
	m map[string]action
}

// newKeymap indexes the console key bindings. An empty binding string is skipped, so
// an operator can disable a pane-local key by clearing it.
func newKeymap(k config.ConsoleKeys) keymap {
	m := map[string]action{}
	put := func(spec string, a action) {
		if spec != "" {
			if _, exists := m[spec]; !exists {
				m[spec] = a
			}
		}
	}
	put(k.Submit, actSubmit)
	put(k.HistoryPrev, actHistoryPrev)
	put(k.HistoryNext, actHistoryNext)
	put(k.ScrollUp, actScrollUp)
	put(k.ScrollDown, actScrollDown)
	put(k.Search, actSearch)
	put(k.Clear, actClear)
	put(k.Reconnect, actReconnect)
	put(k.Disconnect, actDisconnect)
	put(k.Help, actHelp)
	return keymap{m: m}
}

// resolve maps a key string (tea.KeyPressMsg.String()) to its bound action, or
// actNone when unbound.
func (km keymap) resolve(key string) action {
	if a, ok := km.m[key]; ok {
		return a
	}
	return actNone
}
