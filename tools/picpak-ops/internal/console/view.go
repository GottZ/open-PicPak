package console

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// viewColors are the chrome roles the console view styles with. They come from the
// config theme map (no color literal in this package); a missing role resolves to the
// zero color (no styling), so the view never bakes an ANSI index of its own.
type viewColors struct {
	active  color.Color
	working color.Color
	quiet   color.Color
	err     color.Color
	muted   color.Color
	border  color.Color
}

// resolveColors maps theme roles to the console view's palette.
func resolveColors(cfg *config.Config) viewColors {
	c := func(role string) color.Color { return lipgloss.Color(cfg.Theme[role]) }
	return viewColors{
		active:  c("status_active"),
		working: c("status_working"),
		quiet:   c("status_quiet"),
		err:     c("status_error"),
		muted:   c("status_quiet"),
		border:  c("border"),
	}
}

// View renders the pane body: a title bar (host/device/state), the scrollback
// viewport (or the command-catalog overlay when help is toggled), the input line, and
// a key-hint / status footer — sized to the last SetSize. The layout draws the
// surrounding border/title chrome.
func (p *consolePane) View() string {
	w, h := p.Size()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	title := p.renderTitle(w)
	footer := p.renderFooter(w)

	body := p.vp.View()
	if p.helpVisible {
		bodyH := p.vp.Height()
		if bodyH < 1 {
			bodyH = h - 3
		}
		body = p.renderHelp(w, bodyH)
	}

	return strings.Join([]string{title, body, p.input.View(), footer}, "\n")
}

// renderTitle draws the colored state bar.
func (p *consolePane) renderTitle(w int) string {
	st := lipgloss.NewStyle().Foreground(p.stateColor()).Bold(true)
	return st.Render(truncate(p.title(), w))
}

// stateColor maps the link state to a theme role color.
func (p *consolePane) stateColor() color.Color {
	switch statusFor(p.state) {
	case pane.StatusActive:
		return p.colors.active
	case pane.StatusWorking:
		return p.colors.working
	case pane.StatusQuiet:
		return p.colors.quiet
	case pane.StatusError:
		return p.colors.err
	default:
		return p.colors.muted
	}
}

// renderFooter draws the pane-local key hints (all from config), the confirm prompt
// when a device-resetting connect is armed, and the last error.
func (p *consolePane) renderFooter(w int) string {
	if p.confirmArmed {
		warn := fmt.Sprintf("confirm: press %s again to RESET + attach the device", p.cc.Keys.Reconnect)
		return lipgloss.NewStyle().Foreground(p.colors.working).Bold(true).Render(truncate(warn, w))
	}
	hints := fmt.Sprintf("%s connect/reset · %s disconnect · %s help · %s/%s history · %s clear",
		p.cc.Keys.Reconnect, p.cc.Keys.Disconnect, p.cc.Keys.Help,
		p.cc.Keys.HistoryPrev, p.cc.Keys.HistoryNext, p.cc.Keys.Clear)
	if p.lastErr != nil {
		hints = "err: " + p.lastErr.Error() + " · " + hints
		return lipgloss.NewStyle().Foreground(p.colors.err).Render(truncate(hints, w))
	}
	return lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(hints, w))
}

// renderHelp draws the static command catalog (mirrored from console.c) as an overlay,
// clipped to the available body height.
func (p *consolePane) renderHelp(w, h int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Firmware console commands (mirrored from console.c):"))
	b.WriteByte('\n')
	for _, c := range p.catalog {
		head := fmt.Sprintf("  %-8s %s", c.Name, c.Args)
		b.WriteString(truncate(head, w))
		b.WriteByte('\n')
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate("           "+c.Help, w)))
		b.WriteByte('\n')
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if h > 0 && len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// truncate cuts s to a max display width (ANSI-naive; the console output is plain
// text + simple backspace, console.c:69, so a minimal renderer suffices).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
