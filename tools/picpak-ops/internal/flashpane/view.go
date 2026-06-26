package flashpane

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/flash"
)

// viewColors are the chrome roles the flash view styles with — sourced from the config
// theme map (no color literal here); a missing role resolves to the zero color.
type viewColors struct {
	working color.Color
	ok      color.Color
	err     color.Color
	muted   color.Color
	sel     color.Color
}

func resolveColors(cfg *config.Config) viewColors {
	c := func(role string) color.Color { return lipgloss.Color(cfg.Theme[role]) }
	return viewColors{
		working: c("status_working"),
		ok:      c("status_active"),
		err:     c("status_error"),
		muted:   c("status_quiet"),
		sel:     c("focus_border"),
	}
}

// View renders the pane body sized to the last SetSize: the idle picker, or the
// per-device matrix. The layout draws the surrounding border/title.
func (p *flashPane) View() string {
	w, h := p.Size()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	if p.mode == modePick {
		return p.renderPicker(w, h)
	}
	return p.renderMatrix(w, h)
}

// renderPicker lists the VID-gated devices with a cursor + multi-select marks and the
// run hint. Presence is USB-attachment, not liveness (the same honesty as inventory).
func (p *flashPane) renderPicker(w, h int) string {
	var b strings.Builder
	b.WriteString("flash — select target PicPak(s) to write\n")
	b.WriteString("(present = USB-attached; NVS @ preserve offset is never erased)\n\n")

	if len(p.picks) == 0 {
		b.WriteString("No VID-gated PicPak present on any host.\n")
		return b.String()
	}

	for i, row := range p.picks {
		t := row.target
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		mark := "[ ]"
		if p.selected[t.ID] {
			mark = "[x]"
		}
		line := fmt.Sprintf("%s%s %s · %s %s", cursor, mark, t.Host, t.Port(), mapLabel(t))
		if i == p.cursor {
			line = lipgloss.NewStyle().Foreground(p.colors.sel).Render(line)
		}
		b.WriteString(truncate(line, w))
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(
		fmt.Sprintf("%s flash selected · space select · %s console", keyHint(p.fc.Keys.Run), keyHint(p.fc.Keys.Console))))
	return b.String()
}

// renderMatrix draws ONE row per target — never a single batch glyph. Each row shows
// host · port · phase · per-file verified/total · pct · result glyph, then a hint/tail
// box for the selected failed row.
func (p *flashPane) renderMatrix(w, h int) string {
	var b strings.Builder
	done, total := 0, len(p.order)
	for _, id := range p.order {
		if r := p.results[id]; r != nil && r.Phase.Terminal() {
			done++
		}
	}
	b.WriteString(fmt.Sprintf("flash — per-device result (%d/%d done)\n\n", done, total))

	for i, id := range p.order {
		r := p.results[id]
		if r == nil {
			continue
		}
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		glyph, col := p.glyphFor(r)
		prog := fmt.Sprintf("%d/%d", r.Verified, r.NumFiles)
		pct := ""
		if r.Phase == flash.PhaseFlashing {
			pct = fmt.Sprintf(" %d%%", r.Pct)
		}
		status := r.Phase.String()
		if r.Phase == flash.PhaseFailed {
			status = "failed:" + r.Class.String()
		}
		line := fmt.Sprintf("%s%s %s · %s · %s · %s%s",
			cursor, lipgloss.NewStyle().Foreground(col).Render(glyph),
			r.Target.Host, r.Target.Port(), status, prog, pct)
		b.WriteString(truncate(line, w))
		b.WriteByte('\n')
	}

	// Recovery box for the selected failed row.
	if p.cursor >= 0 && p.cursor < len(p.order) {
		if r := p.results[p.order[p.cursor]]; r != nil && r.Phase == flash.PhaseFailed {
			b.WriteByte('\n')
			b.WriteString(p.renderFailureBox(r, w))
		}
	}

	b.WriteByte('\n')
	b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(
		fmt.Sprintf("%s retry · %s abort · %s console", keyHint(p.fc.Keys.Retry), keyHint(p.fc.Keys.Abort), keyHint(p.fc.Keys.Console))))
	return b.String()
}

// renderFailureBox shows the fail class, the recovery hint, and the bounded esptool
// output tail for one failed device.
func (p *flashPane) renderFailureBox(r *flash.DeviceResult, w int) string {
	boxW := w - 2
	if boxW < 10 {
		boxW = 10
	}
	title := fmt.Sprintf("%s failed (%s)", r.Target.Port(), r.Class.String())
	body := r.Hint
	if len(r.Tail) > 0 {
		body += "\n" + strings.Join(r.Tail, "\n")
	}
	if strings.TrimSpace(body) == "" {
		body = "(no output captured)"
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(p.colors.err).
		Foreground(p.colors.err).
		Padding(0, 1).
		Width(boxW)
	return box.Render(lipgloss.NewStyle().Bold(true).Render(title) + "\n" + body)
}

// glyphFor maps a device row to its status glyph + color.
func (p *flashPane) glyphFor(r *flash.DeviceResult) (string, color.Color) {
	switch r.Phase {
	case flash.PhaseDone:
		return "✓", p.colors.ok
	case flash.PhaseFailed:
		return "✗", p.colors.err
	default:
		return "◐", p.colors.working
	}
}

// mapLabel renders the device identity honestly (mapped serial/label, else unmapped).
func mapLabel(t flash.Target) string {
	if t.Device.Serial != "" {
		return "mapped:" + t.Device.Serial
	}
	if t.Device.Label != "" {
		return "mapped:" + t.Device.Label
	}
	return "unmapped"
}

func keyHint(k string) string {
	if k == "" {
		return "—"
	}
	return k
}

func itoa(n int) string { return strconv.Itoa(n) }

// truncate cuts s to a max display width (ANSI-naive; flash rows are plain text).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
