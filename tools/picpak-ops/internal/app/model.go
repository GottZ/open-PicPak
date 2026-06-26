// Package app is the wm-shell runtime: the single Bubble Tea root Model that owns
// the terminal, the pane registry, the layout/tabs, the global keymap, and the
// graceful teardown of every background resource. Feature panes plug in by
// implementing pane.Pane and registering a factory in main.go; this package
// imports no transport (ssh, pgx, exec) — only the pane contract and config.
//
// The root is the only tea.Model. Panes are composed string-renderers it drives
// (wm-shell §2.1/§4.1). Background liveness is a goroutine bound to the injected
// Sender; the root routes each goroutine's addressed PaneMsg to its one pane,
// focused or hidden (K2).
package app

import (
	"context"
	"image/color"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/keys"
	"github.com/open-picpak/picpak-ops/internal/layout"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// Status glyphs are UI runes (not colors/hosts/paths/identifiers), kept as the
// single in-package constant location for the chrome markers. Their COLORS come
// from the config theme; only the glyph shapes live here.
const (
	glyphActive  = "●"
	glyphWorking = "◐"
	glyphQuiet   = "○"
	glyphIdle    = "·"
	glyphError   = "✗"
	dirtyMarker  = "*"
)

// Model is the single wm-shell root (held by POINTER — §4.5 pointer invariant, so
// SetSender after NewProgram mutates the instance the program holds).
type Model struct {
	cfg  *config.Config
	send pane.Sender // wrapper over (*tea.Program).Send; set after NewProgram

	rootCtx context.Context
	cancel  context.CancelFunc

	reg    *paneRegistry
	layout *layout.Model
	keys   keys.Map
	help   helpState

	width, height int
	status        string
	fatal         error
	quitting      bool

	// quitArmed tracks the quit-guard (K5): a first quit key with a Working pane
	// arms a confirm; the second confirms. Cleared by any non-confirm key.
	quitArmed bool

	// reloadOpts/reloadPath let the shell re-issue the config watch Cmd after each
	// ReloadMsg (config.WatchCmd is one-shot per design).
	reloadOpts config.Opts
	reloadPath string
}

type helpState struct {
	visible bool
}

// New builds the root Model from a loaded config. It MUST return *Model (§4.5);
// the program holds this pointer and SetSender mutates m.send afterward.
//
// rootCtx is the application context (cancelled on quit); per-pane contexts are
// children of it. send is nil until SetSender — safe because panes spawn during
// Run() (after SetSender), so no goroutine captures a nil Sender.
func New(cfg *config.Config, rootCtx context.Context, cancel context.CancelFunc) *Model {
	m := &Model{
		cfg:     cfg,
		rootCtx: rootCtx,
		cancel:  cancel,
		keys:    keys.New(mustKeymap(cfg)),
		help:    helpState{},
	}
	tabs := layout.NewTabs()
	m.layout = layout.New(tabs, resolveTheme(cfg), resolveLayoutOpts(cfg))
	// The registry needs the Sender; it is injected via SetSender before Run, and
	// the registry reads m.send lazily through a small indirection so the not-yet-
	// set Sender is fine until the first spawn (which happens inside Run).
	m.reg = newRegistry(rootCtx, m.senderBridge(), cfg)
	return m
}

// SetSender injects the goroutine-safe bridge to the program message loop. Called
// once, after NewProgram, mutating THIS instance (pointer invariant). The registry
// already holds m.senderBridge(), which forwards to m.send, so a single SetSender
// makes every future pane's Sender live.
func (m *Model) SetSender(send pane.Sender) { m.send = send }

// senderBridge returns a stable Sender closure that forwards to the current
// m.send. Panes capture this at spawn; because it reads m.send at call time, the
// SetSender-after-NewProgram ordering works without re-wiring panes.
func (m *Model) senderBridge() pane.Sender {
	return func(msg tea.Msg) {
		if m.send != nil {
			m.send(msg)
		}
	}
}

// RegisterFactory binds a pane-kind factory (main.go calls this before Run).
func (m *Model) RegisterFactory(kind pane.PaneKind, f PaneFactory) {
	m.reg.register(kind, f)
}

// SetReload records the watch inputs so the shell can re-arm config.WatchCmd after
// each ReloadMsg (the watch Cmd is one-shot). path=="" disables re-arming.
func (m *Model) SetReload(path string, opts config.Opts) {
	m.reloadPath = path
	m.reloadOpts = opts
}

// Init spawns the startup panes (or a launcher when none) and arms the config
// watch. It runs inside Run(), after SetSender, so the Sender is live.
func (m *Model) Init() tea.Cmd {
	var cmds []tea.Cmd

	specs := m.cfg.UI.StartupPanes
	if len(specs) == 0 {
		// Land on a launcher so the app always has somewhere to be (OQ#3 default).
		if cmd := m.spawn(pane.KindLauncher, nil, true); cmd != nil {
			cmds = append(cmds, cmd)
		}
	} else {
		for _, s := range specs {
			if cmd := m.spawn(s.Kind, s.Args, s.Focus); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	if m.reloadPath != "" && m.cfg.Reload.Watch {
		opts := m.reloadOpts
		opts.ReloadDebounce = m.cfg.Reload.Debounce.D()
		if wc := config.WatchCmd(m.rootCtx, m.reloadPath, opts); wc != nil {
			cmds = append(cmds, wc)
		}
	}

	return tea.Batch(cmds...)
}

// mustKeymap parses the config keymap; a malformed keymap was already rejected by
// config validation at Load, so a parse error here is impossible for a loaded
// config. It returns nil on error (keys.New tolerates nil → pass-all-to-pane).
func mustKeymap(cfg *config.Config) *config.Keymap {
	km, err := config.BuildKeymap(cfg.Keys)
	if err != nil {
		return nil
	}
	return km
}

// resolveTheme maps the config theme-role color map to a layout.Theme. A missing
// role resolves to the zero color (no styling) — the engine never bakes its own
// default ANSI index (air-gap: no color literal in shell/layout code; the only
// sanctioned color-string location is the config defaults table).
func resolveTheme(cfg *config.Config) layout.Theme {
	col := func(role string) color.Color {
		return lipgloss.Color(cfg.Theme[role])
	}
	return layout.Theme{
		Border:       col("border"),
		FocusBorder:  col("focus_border"),
		TabActive:    col("tab_active"),
		TabInactive:  col("tab_inactive"),
		StatusActive: col("status_active"),
		StatusWork:   col("status_working"),
		StatusQuiet:  col("status_quiet"),
		StatusError:  col("status_error"),
		Dirty:        col("dirty"),
		GlyphActive:  glyphActive,
		GlyphWorking: glyphWorking,
		GlyphQuiet:   glyphQuiet,
		GlyphIdle:    glyphIdle,
		GlyphError:   glyphError,
		DirtyMarker:  dirtyMarker,
	}
}

// resolveLayoutOpts maps ui.* chrome config to layout.Options.
func resolveLayoutOpts(cfg *config.Config) layout.Options {
	return layout.Options{
		TabBar:    cfg.UI.TabBar,
		StatusBar: cfg.UI.StatusBar,
	}
}
