package keys

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// keyPress builds a printable-rune key press whose String() is the single rune.
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// ctrl builds a ctrl-modified key press (String() == "ctrl+<r>").
func ctrl(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

func TestSingleKeyResolves(t *testing.T) {
	km, err := config.BuildKeymap(config.Keys{
		Quit: []string{"q", "ctrl+c"},
		Help: []string{"?"},
	})
	if err != nil {
		t.Fatalf("BuildKeymap: %v", err)
	}
	m := New(km)

	if got := m.Resolve(keyPress('q')); got.Verdict != GlobalAction || got.Action != "quit" {
		t.Fatalf("q → %+v, want quit", got)
	}
	if got := m.Resolve(ctrl('c')); got.Verdict != GlobalAction || got.Action != "quit" {
		t.Fatalf("ctrl+c → %+v, want quit", got)
	}
	if got := m.Resolve(keyPress('?')); got.Verdict != GlobalAction || got.Action != "help" {
		t.Fatalf("? → %+v, want help", got)
	}
	// An unbound key passes to the focused pane.
	if got := m.Resolve(keyPress('x')); got.Verdict != PassToPane {
		t.Fatalf("x → %+v, want PassToPane", got)
	}
}

// TestChordResolves proves the K7 prefix machine: a two-key chord resolves to its
// action, and a non-matching second key clears the prefix (REQUIRED test).
func TestChordResolves(t *testing.T) {
	// "ctrl+b z" is a chord (space-separated) bound to spawn.build.
	km, err := config.BuildKeymap(config.Keys{
		Quit:  []string{"q"},
		Spawn: map[string][]string{"build": {"ctrl+b z"}},
	})
	if err != nil {
		t.Fatalf("BuildKeymap: %v", err)
	}
	m := New(km)

	// First key (the prefix) is swallowed and arms the pending prefix.
	if got := m.Resolve(ctrl('b')); got.Verdict != Swallowed {
		t.Fatalf("prefix ctrl+b → %+v, want Swallowed", got)
	}
	if m.PendingPrefix() != "ctrl+b" {
		t.Fatalf("pending prefix = %q, want ctrl+b", m.PendingPrefix())
	}
	// Matching second key completes the chord → the action fires.
	if got := m.Resolve(keyPress('z')); got.Verdict != GlobalAction || got.Action != "spawn.build" {
		t.Fatalf("chord ctrl+b z → %+v, want spawn.build", got)
	}
	if m.PendingPrefix() != "" {
		t.Fatalf("pending prefix not cleared after chord: %q", m.PendingPrefix())
	}
}

func TestChordNonMatchClearsPrefix(t *testing.T) {
	km, err := config.BuildKeymap(config.Keys{
		Quit:  []string{"q"},
		Spawn: map[string][]string{"build": {"ctrl+b z"}},
	})
	if err != nil {
		t.Fatalf("BuildKeymap: %v", err)
	}
	m := New(km)

	if got := m.Resolve(ctrl('b')); got.Verdict != Swallowed {
		t.Fatalf("prefix ctrl+b → %+v, want Swallowed", got)
	}
	// A NON-matching second key clears the prefix and is offered fresh to the
	// single-key layer. 'q' is bound to quit → it resolves to quit, not eaten.
	if got := m.Resolve(keyPress('q')); got.Verdict != GlobalAction || got.Action != "quit" {
		t.Fatalf("non-match second key → %+v, want quit (prefix cleared, key re-offered)", got)
	}
	if m.PendingPrefix() != "" {
		t.Fatalf("pending prefix not cleared after non-match: %q", m.PendingPrefix())
	}
	// A non-matching second key that is also unbound passes to the pane.
	_ = m.Resolve(ctrl('b'))
	if got := m.Resolve(keyPress('x')); got.Verdict != PassToPane {
		t.Fatalf("non-match unbound second key → %+v, want PassToPane", got)
	}
	if m.PendingPrefix() != "" {
		t.Fatalf("pending prefix lingered: %q", m.PendingPrefix())
	}
}

func TestNilKeymapPassesAll(t *testing.T) {
	m := New(nil)
	if got := m.Resolve(keyPress('q')); got.Verdict != PassToPane {
		t.Fatalf("nil keymap should pass all to pane, got %+v", got)
	}
}
