package logs

import (
	"fmt"
	"hash/fnv"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// viewColors are the chrome roles the logs view styles with — sourced from the config
// theme map (no color literal here); a missing role resolves to the zero color (no
// styling). The per-line content color is the separate config palette (colorize_by), not
// a theme role, so an operator can tune device colors without touching the theme.
type viewColors struct {
	gap     color.Color // boot-gap divider
	suspect color.Color // suspect marker
	muted   color.Color // footer / hints
	sel     color.Color // overlay cursor row
	border  color.Color
}

func resolveColors(cfg *config.Config) viewColors {
	c := func(role string) color.Color { return lipgloss.Color(cfg.Theme[role]) }
	return viewColors{
		gap:     c("status_working"),
		suspect: c("status_error"),
		muted:   c("status_quiet"),
		sel:     c("focus_border"),
		border:  c("border"),
	}
}

// resolveLocation resolves logs.timezone to a *time.Location (TIMESTAMPTZ is rendered
// zone-aware so forensics is not misled by server-UTC shown as local). "" / "Local" →
// the operator's local zone; an unknown name falls back to Local rather than failing.
func resolveLocation(tz string) *time.Location {
	if tz == "" || tz == "Local" {
		return time.Local
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.Local
}

// View renders the pane body sized to the last SetSize: the no-DB state, the device
// filter overlay, or the scrollback viewport + a status/hints footer. The layout draws
// the surrounding border/title.
func (p *logsPane) View() string {
	w, h := p.Size()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	if p.store == nil {
		return p.renderNoDB(w)
	}
	if p.filter.open {
		return p.renderFilterOverlay(w, h)
	}
	return p.vp.View() + "\n" + p.renderFooter(w)
}

// renderNoDB is the explicit "no database configured" state (nil pool / empty DSN) — a
// static, honest message, never a crash.
func (p *logsPane) renderNoDB(w int) string {
	muted := lipgloss.NewStyle().Foreground(p.colors.muted)
	return strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Render("logs — no database configured"),
		muted.Render(truncate("Set [database].dsn to read the logs hypertable.", w)),
		muted.Render(truncate("The bench cockpit (build / hosts / flash / console) runs without a DB.", w)),
	}, "\n")
}

// renderFilterOverlay draws the device multiselect (space toggles, enter applies, esc
// cancels), clipped to the available height.
func (p *logsPane) renderFilterOverlay(w, h int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(
		truncate("Filter devices — ↑/↓ move · space toggle · enter apply · esc cancel", w)))
	b.WriteByte('\n')
	if len(p.filter.serials) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("(no device serials known yet)"))
		return b.String()
	}
	maxRows := h - 2
	if maxRows < 1 {
		maxRows = 1
	}
	for i, s := range p.filter.serials {
		if i >= maxRows {
			b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(
				fmt.Sprintf("… %d more", len(p.filter.serials)-maxRows)))
			break
		}
		check := "[ ]"
		if p.filter.selected[s] {
			check = "[x]"
		}
		cursor := "  "
		if i == p.filter.cursor {
			cursor = "▸ "
		}
		label := s
		if p.cache != nil {
			if d, ok := p.cache.BySerial(s); ok && d.Label != "" {
				label = s + " · " + d.Label
			}
		}
		line := fmt.Sprintf("%s%s %s", cursor, check, label)
		if i == p.filter.cursor {
			line = lipgloss.NewStyle().Foreground(p.colors.sel).Bold(true).Render(truncate(line, w))
		} else {
			line = truncate(line, w)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderFooter shows the follow/paused chrome, the active scope, the line count and the
// pane-local key hints — or the non-fatal reconnecting error badge.
func (p *logsPane) renderFooter(w int) string {
	if p.err != nil {
		return lipgloss.NewStyle().Foreground(p.colors.suspect).Render(
			truncate("DB error (reconnecting): "+p.err.Error(), w))
	}
	mode := "paused"
	if p.follow {
		mode = "following"
	}
	serials := p.store.FilterSerials()
	scope := "all devices"
	switch len(serials) {
	case 1:
		scope = serials[0]
	default:
		if len(serials) > 1 {
			scope = fmt.Sprintf("%d devices", len(serials))
		}
	}
	srcScope := "all sources"
	if srcs := p.store.FilterSources(); len(srcs) > 0 {
		srcScope = strings.Join(srcs, ",")
	}
	status := strings.Join([]string{mode, scope, srcScope, fmt.Sprintf("%d lines", p.store.Len())}, " · ")
	hints := fmt.Sprintf("%s follow · %s filter · %s source · %s reload",
		p.keys.follow, p.keys.deviceFilter, p.keys.sourceCycle, p.keys.reload)
	return lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(status+"  |  "+hints, w))
}

// renderLine styles one Line: synthetic markers get a distinct (config-theme) style;
// device content lines are colorized by the config palette (colorize_by serial/source),
// with a plain no-color fallback when the palette is empty or colorize_by=none.
func (p *logsPane) renderLine(ln Line) string {
	text := p.lineText(ln)
	switch ln.Synthetic {
	case SynthGap:
		return lipgloss.NewStyle().Foreground(p.colors.gap).Italic(true).Render(text)
	case SynthSuspect:
		return lipgloss.NewStyle().Foreground(p.colors.suspect).Bold(true).Render(text)
	}
	if col, ok := p.colorFor(ln); ok {
		return lipgloss.NewStyle().Foreground(col).Render(text)
	}
	return text
}

// lineText is the uncolored text of a Line (also the clipboard form): timestamp · serial
// (or serial·label) · [source] · text. The timestamp is rendered in logs.timezone with
// logs.timestamp_format.
func (p *logsPane) lineText(ln Line) string {
	var parts []string
	if ts := p.timestamp(ln.Time); ts != "" {
		parts = append(parts, ts)
	}
	parts = append(parts, p.serialLabel(ln.Serial))
	if ln.Synthetic != SynthNone {
		parts = append(parts, ln.Text)
		return strings.Join(parts, " ")
	}
	if ln.Source != "" {
		parts = append(parts, "["+ln.Source+"]")
	}
	parts = append(parts, ln.Text)
	return strings.Join(parts, " ")
}

// plainLine is the clipboard form of a Line (same as lineText — no ANSI).
func (p *logsPane) plainLine(ln Line) string { return p.lineText(ln) }

// timestamp formats a row time in the configured zone+layout ("" when no layout set).
func (p *logsPane) timestamp(t time.Time) string {
	if t.IsZero() || p.cfg.TimestampFormat == "" {
		return ""
	}
	if p.loc != nil {
		t = t.In(p.loc)
	}
	return t.Format(p.cfg.TimestampFormat)
}

// serialLabel renders a serial as "serial · label" when the fleet cache knows a label,
// else the bare serial.
func (p *logsPane) serialLabel(serial string) string {
	if p.cache != nil {
		if d, ok := p.cache.BySerial(serial); ok && d.Label != "" {
			return serial + "·" + d.Label
		}
	}
	return serial
}

// colorFor picks a stable palette color for a line by hashing its colorize key (serial or
// source). Returns (nil,false) for colorize_by=none, an unknown mode, an empty palette, or
// an empty key — the no-color fallback.
func (p *logsPane) colorFor(ln Line) (color.Color, bool) {
	if len(p.cfg.Palette) == 0 {
		return nil, false
	}
	var key string
	switch p.cfg.ColorizeBy {
	case "serial":
		key = ln.Serial
	case "source":
		key = ln.Source
	default:
		return nil, false
	}
	if key == "" {
		return nil, false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	idx := int(h.Sum32() % uint32(len(p.cfg.Palette)))
	return lipgloss.Color(p.cfg.Palette[idx]), true
}

// truncate cuts s to a max display width (ANSI-naive; logs cells are plain text until
// renderLine wraps them).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
