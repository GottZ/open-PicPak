package config

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/key"
)

// ChordSpec is a two-keystroke chord, e.g. {"ctrl+b","z"} for "ctrl+b z". config
// PARSES space-containing keyspecs into this shape; it does NOT hold the prefix
// STATE (that is wm-shell's prefix matcher, K7). Chords are parseable but off by
// default — the default keymap ships single-key bindings only.
type ChordSpec struct {
	Prefix string
	Key    string
}

func (c ChordSpec) String() string { return c.Prefix + " " + c.Key }

// Keymap is the parsed, validated keybinding set built from the wm-shell §5
// keys.* namespace. Single-key specs become key.Binding; space-containing specs
// become ChordSpec. It carries no key→action runtime resolution (that needs
// wm-shell-side prefix state for chords).
type Keymap struct {
	// Bindings maps an action name to its single-key key.Binding (a binding may
	// list several alternative single keys via WithKeys).
	Bindings map[string]key.Binding
	// Chords maps an action name to its parsed chord specs (multi-key only).
	Chords map[string][]ChordSpec
}

// BuildKeymap parses the global keys.* namespace into a Keymap. Single keystrokes
// (no space) collapse into one key.Binding per action; any space-containing spec
// becomes a ChordSpec. It returns an error if a binding is malformed.
//
// K7: the no-double-bind check is scoped per pane-scope. The global chrome keys
// here all live in the single "global" scope; pane action keymaps (console.keys,
// ota.keys, …) are separate scopes and may reuse a keystroke legally.
func BuildKeymap(k Keys) (*Keymap, error) {
	km := &Keymap{
		Bindings: map[string]key.Binding{},
		Chords:   map[string][]ChordSpec{},
	}
	add := func(action string, specs ...string) error {
		return km.addAction(action, specs...)
	}

	if err := add("quit", k.Quit...); err != nil {
		return nil, err
	}
	if err := add("help", k.Help...); err != nil {
		return nil, err
	}
	if err := add("next_tab", k.NextTab...); err != nil {
		return nil, err
	}
	if err := add("prev_tab", k.PrevTab...); err != nil {
		return nil, err
	}
	if err := add("close_pane", k.ClosePane...); err != nil {
		return nil, err
	}
	if err := add("split", k.Split...); err != nil {
		return nil, err
	}
	for kind, specs := range k.Spawn {
		if err := add("spawn."+kind, specs...); err != nil {
			return nil, err
		}
	}

	return km, nil
}

// addAction parses one action's specs, splitting single keys from chords.
func (km *Keymap) addAction(action string, specs ...string) error {
	var singles []string
	for _, raw := range specs {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			return fmt.Errorf("keys: action %q has an empty binding", action)
		}
		if cs, isChord, err := parseChord(spec); err != nil {
			return err
		} else if isChord {
			km.Chords[action] = append(km.Chords[action], cs)
		} else {
			singles = append(singles, spec)
		}
	}
	if len(singles) > 0 {
		km.Bindings[action] = key.NewBinding(
			key.WithKeys(singles...),
			key.WithHelp(strings.Join(singles, "/"), action),
		)
	}
	return nil
}

// parseChord classifies a keyspec. A spec with exactly one space is a two-key
// chord (prefix + key); no space is a single key; more than one space is an
// error (no support for 3+ key chords).
func parseChord(spec string) (ChordSpec, bool, error) {
	if !strings.Contains(spec, " ") {
		return ChordSpec{}, false, nil
	}
	fields := strings.Fields(spec)
	if len(fields) != 2 {
		return ChordSpec{}, false, fmt.Errorf("keys: chord %q must be exactly two keystrokes (prefix + key)", spec)
	}
	return ChordSpec{Prefix: fields[0], Key: fields[1]}, true, nil
}

// scopedKeyspec normalizes a fully-specified binding (single key, or a complete
// prefix+key chord) into a comparable string for the no-double-bind check.
func scopedKeyspec(spec string) string {
	return strings.Join(strings.Fields(spec), " ")
}

// ValidateKeyScope checks that no fully-specified binding is bound to two actions
// within the SAME scope (K7 / validation rule 7). actions maps an action name to
// its raw specs; the returned error names the offending keyspec and actions.
func ValidateKeyScope(scope string, actions map[string][]string) error {
	owner := map[string]string{} // keyspec → first action that claimed it
	// Iterate deterministically for stable error messages.
	names := make([]string, 0, len(actions))
	for a := range actions {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, action := range names {
		for _, raw := range actions[action] {
			spec := scopedKeyspec(raw)
			if spec == "" {
				continue
			}
			if prev, ok := owner[spec]; ok && prev != action {
				return fmt.Errorf("keys: keyspec %q is bound to both %q and %q in scope %q", spec, prev, action, scope)
			}
			owner[spec] = action
		}
	}
	return nil
}

// validateKeymap is the validate.go hook (rule 7): parse the global keymap (so a
// malformed binding is a load error) and run the per-scope no-double-bind check
// over the global chrome scope. Pane-scoped action maps (console/ota) are
// distinct scopes; this validates the global scope wm-shell dispatches first.
func (c *Config) validateKeymap(add func(string, ...any)) {
	if _, err := BuildKeymap(c.Keys); err != nil {
		add("%v", err)
	}

	global := map[string][]string{
		"quit":       c.Keys.Quit,
		"help":       c.Keys.Help,
		"next_tab":   c.Keys.NextTab,
		"prev_tab":   c.Keys.PrevTab,
		"close_pane": c.Keys.ClosePane,
		"split":      c.Keys.Split,
	}
	for kind, specs := range c.Keys.Spawn {
		global["spawn."+kind] = specs
	}
	if err := ValidateKeyScope("global", global); err != nil {
		add("%v", err)
	}

	// The OTA pane keymap is a distinct (pane) scope; validate within itself.
	if len(c.OTA.Keys) > 0 {
		if err := ValidateKeyScope("ota", c.OTA.Keys); err != nil {
			add("%v", err)
		}
	}

	// The telemetry pane keymap is a distinct (pane) scope; validate within itself.
	if len(c.Telemetry.Keys) > 0 {
		if err := ValidateKeyScope("telemetry", c.Telemetry.Keys); err != nil {
			add("%v", err)
		}
	}

	// The logs pane keymap is a distinct (pane) scope; validate within itself.
	if len(c.Logs.Keys) > 0 {
		if err := ValidateKeyScope("logs", c.Logs.Keys); err != nil {
			add("%v", err)
		}
	}
}
