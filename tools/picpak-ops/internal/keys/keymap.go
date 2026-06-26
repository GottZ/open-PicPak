// Package keys is the wm-shell global keymap and chord prefix-state machine.
//
// It consumes the config-owned, already-parsed keymap (config.Keymap: single-key
// key.Bindings + multi-key ChordSpecs) and turns a sequence of key presses into a
// resolved global action or the verdict "pass this key to the focused pane".
//
// Two resolution layers, in this order:
//
//  1. Chord prefix machine (K7). If a key matches a registered chord prefix, the
//     map enters a pending state and SWALLOWS the key (it is not a global action
//     and is not passed to the pane). The next key resolves the chord: a match
//     fires the chord's action; a non-match clears the prefix (the second key is
//     then offered to the single-key layer as a fresh press). Default keymaps ship
//     single-key only, so the pending state stays empty unless an operator
//     configures a chord.
//  2. Single-key layer. A key that matches a global single-key binding resolves to
//     that action; otherwise the verdict is "pass to pane".
//
// Global chrome actions always win over a pane (dispatch step 2 in the shell), so
// a pane can never swallow quit/tab/close/spawn. Pane-local action keys live in
// each pane's own keymap and are matched by the pane while focused (K7 per-pane
// scope), never here.
package keys

import (
	"sort"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// Verdict classifies a resolved key press.
type Verdict int

const (
	// PassToPane means the shell did not consume the key; the focused pane gets it.
	PassToPane Verdict = iota
	// GlobalAction means the key resolved to a global chrome action (see Action).
	GlobalAction
	// Swallowed means the key was consumed by the chord prefix machine (it armed a
	// pending prefix). It is neither a global action nor passed to the pane.
	Swallowed
)

// Result is the outcome of resolving one key press.
type Result struct {
	Verdict Verdict
	// Action is the resolved action name (e.g. "quit", "next_tab", "spawn.build")
	// when Verdict==GlobalAction; empty otherwise.
	Action string
}

// Map is the global keymap plus the live chord prefix state. It is owned by the
// shell root (one instance) and mutated only on the message loop, so it needs no
// locking. Build it with New; resolve presses with Resolve.
type Map struct {
	km *config.Keymap

	// chordPrefixes lists every distinct prefix keystroke that begins a chord, so
	// a single press can be classified as "starts a chord" in O(1).
	chordPrefixes map[string]struct{}

	// pendingPrefix is the armed chord prefix ("" when idle). When non-empty the
	// next press is resolved against chords whose Prefix == pendingPrefix.
	pendingPrefix string
}

// New builds the resolver from a config-parsed keymap. The keymap is the single
// source of bindings (config axis); New only indexes the chord prefixes for the
// state machine. A nil keymap yields a resolver that passes every key to the pane.
func New(km *config.Keymap) Map {
	m := Map{km: km, chordPrefixes: map[string]struct{}{}}
	if km == nil {
		return m
	}
	for _, specs := range km.Chords {
		for _, cs := range specs {
			m.chordPrefixes[cs.Prefix] = struct{}{}
		}
	}
	return m
}

// PendingPrefix returns the currently armed chord prefix ("" when idle). The
// shell surfaces it in the status line so the operator sees a half-typed chord.
func (m *Map) PendingPrefix() string { return m.pendingPrefix }

// Reset clears any pending chord prefix (e.g. on focus change or Escape).
func (m *Map) Reset() { m.pendingPrefix = "" }

// Resolve classifies one key press. It runs the chord machine first, then the
// single-key layer. It mutates the pending-prefix state and returns the verdict.
func (m *Map) Resolve(k tea.KeyPressMsg) Result {
	if m.km == nil {
		return Result{Verdict: PassToPane}
	}
	ks := k.String()

	// --- chord machine ---------------------------------------------------------
	if m.pendingPrefix != "" {
		// A prefix is armed: try to complete the chord with this key.
		prefix := m.pendingPrefix
		m.pendingPrefix = "" // a chord is always two keys; clear regardless of outcome
		if action, ok := m.matchChord(prefix, ks); ok {
			return Result{Verdict: GlobalAction, Action: action}
		}
		// Non-match clears the prefix; fall through and offer this key to the
		// single-key layer as a fresh press (a chord miss must not eat the key).
	}

	// Does this key START a chord? If so, arm and swallow.
	if _, ok := m.chordPrefixes[ks]; ok {
		m.pendingPrefix = ks
		return Result{Verdict: Swallowed}
	}

	// --- single-key layer ------------------------------------------------------
	if action, ok := m.matchSingle(k); ok {
		return Result{Verdict: GlobalAction, Action: action}
	}
	return Result{Verdict: PassToPane}
}

// matchChord returns the action a (prefix,key) chord resolves to, if any. When
// several actions share a prefix it picks the lowest action name deterministically.
func (m *Map) matchChord(prefix, ks string) (string, bool) {
	names := make([]string, 0, len(m.km.Chords))
	for action := range m.km.Chords {
		names = append(names, action)
	}
	sort.Strings(names)
	for _, action := range names {
		for _, cs := range m.km.Chords[action] {
			if cs.Prefix == prefix && cs.Key == ks {
				return action, true
			}
		}
	}
	return "", false
}

// matchSingle returns the global action a single key resolves to, if any. It uses
// the config-built key.Bindings (key.Matches handles multi-key alternatives per
// action). When several actions could match, the lowest action name wins
// deterministically — config's per-scope no-double-bind check makes overlap a load
// error in practice, so this is only a tie-break for safety.
func (m *Map) matchSingle(k tea.KeyPressMsg) (string, bool) {
	names := make([]string, 0, len(m.km.Bindings))
	for action := range m.km.Bindings {
		names = append(names, action)
	}
	sort.Strings(names)
	for _, action := range names {
		if key.Matches(k, m.km.Bindings[action]) {
			return action, true
		}
	}
	return "", false
}

// Bindings returns the underlying single-key bindings (for the help overlay).
func (m *Map) Bindings() map[string]key.Binding {
	if m.km == nil {
		return nil
	}
	return m.km.Bindings
}

// HelpBindings returns the global single-key bindings in a stable order for a
// help overlay (bubbles/help consumes a []key.Binding).
func (m *Map) HelpBindings() []key.Binding {
	if m.km == nil {
		return nil
	}
	names := make([]string, 0, len(m.km.Bindings))
	for a := range m.km.Bindings {
		names = append(names, a)
	}
	sort.Strings(names)
	out := make([]key.Binding, 0, len(names))
	for _, a := range names {
		out = append(out, m.km.Bindings[a])
	}
	return out
}
