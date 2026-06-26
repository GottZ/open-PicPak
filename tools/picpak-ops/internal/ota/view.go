package ota

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// viewColors are the chrome roles the OTA view styles with — sourced from the config
// theme map (no color literal here); a missing role resolves to the zero color.
type viewColors struct {
	ok      color.Color
	warning color.Color
	err     color.Color
	muted   color.Color
	sel     color.Color
	border  color.Color
}

func resolveColors(cfg *config.Config) viewColors {
	c := func(role string) color.Color { return lipgloss.Color(cfg.Theme[role]) }
	return viewColors{
		ok:      c("status_active"),
		warning: c("status_working"),
		err:     c("status_error"),
		muted:   c("status_quiet"),
		sel:     c("focus_border"),
		border:  c("border"),
	}
}

// View renders the pane body sized to the last SetSize: no-DB / state, plus the
// direct-write banner, the active form overlay, and a status/footer line. The layout
// chrome draws the surrounding border/title.
func (p *otaPane) View() string {
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

	var b strings.Builder
	if p.directWrite {
		b.WriteString(p.renderDirectBanner(w))
		b.WriteByte('\n')
	}

	if p.form != nil {
		b.WriteString(p.renderForm(w))
		b.WriteByte('\n')
		b.WriteString(p.renderStatus(w))
		return b.String()
	}

	b.WriteString(p.renderVersions(w))
	b.WriteByte('\n')
	b.WriteString(p.renderDevices(w))
	b.WriteByte('\n')
	b.WriteString(p.renderFooter(w))
	b.WriteByte('\n')
	b.WriteString(p.renderStatus(w))
	return b.String()
}

// renderNoDB is the explicit "no database configured" state (nil pool / empty DSN).
func (p *otaPane) renderNoDB(w int) string {
	muted := lipgloss.NewStyle().Foreground(p.colors.muted)
	return strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Render("ota — no database configured"),
		muted.Render(truncate("Set [database].dsn to read firmware versions, channels and rollouts.", w)),
		muted.Render(truncate("The bench cockpit (build / hosts / flash / console) runs without a DB.", w)),
	}, "\n")
}

// renderDirectBanner is the loud red "DIRECT-WRITE MODE" warning shown while the interim
// DirectPGXWriter is active (bypassing the backend authority).
func (p *otaPane) renderDirectBanner(w int) string {
	style := lipgloss.NewStyle().Foreground(p.colors.err).Bold(true)
	return style.Render(truncate("⚠ DIRECT-WRITE MODE — bypassing backend authority (interim direct-pgx writes)", w))
}

// renderVersions is the firmware_versions table (the active table gets a heading marker).
func (p *otaPane) renderVersions(w int) string {
	var b strings.Builder
	b.WriteString(p.tableHeading("Firmware versions", tableVersions))
	b.WriteByte('\n')
	if len(p.state.Versions) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("  (none registered)"))
		return b.String()
	}
	for i, v := range p.state.Versions {
		cursor := "  "
		if p.active == tableVersions && i == p.verCur {
			cursor = "▸ "
		}
		short := v.SHA256
		if len(short) > 12 {
			short = short[:12]
		}
		line := fmt.Sprintf("%s%-12s %s  %d B", cursor, v.Version, short, v.SizeBytes)
		if p.active == tableVersions && i == p.verCur {
			line = lipgloss.NewStyle().Foreground(p.colors.sel).Bold(true).Render(truncate(line, w))
		} else {
			line = truncate(line, w)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderDevices is the per-device target-vs-running table: serial/label · channel ·
// running → target (source) · state · behind/last-seen.
func (p *otaPane) renderDevices(w int) string {
	now := time.Now()
	var b strings.Builder
	b.WriteString(p.tableHeading("Devices (target vs running)", tableDevices))
	b.WriteByte('\n')
	if len(p.state.Devices) == 0 {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("  (no devices reported / configured)"))
		return b.String()
	}
	for i, d := range p.state.Devices {
		cursor := "  "
		if p.active == tableDevices && i == p.devCur {
			cursor = "▸ "
		}
		name := d.Serial
		if d.Label != "" {
			name = d.Serial + "·" + d.Label
		}
		running := d.RunningVer
		if running == "" {
			running = "?"
		}
		target := d.Resolved.Version
		if target == "" {
			target = "—"
		}
		state := "—"
		if d.Rollout != nil {
			state = d.Rollout.State
		}
		verdict := p.deviceVerdict(d, now)
		line := fmt.Sprintf("%s%-16s %-7s %s→%s [%s] %s %s",
			cursor, name, d.Channel, running, target, d.Resolved.Source, state, verdict)
		switch {
		case p.active == tableDevices && i == p.devCur:
			line = lipgloss.NewStyle().Foreground(p.colors.sel).Bold(true).Render(truncate(line, w))
		case d.Behind:
			line = lipgloss.NewStyle().Foreground(p.colors.warning).Render(truncate(line, w))
		default:
			line = truncate(line, w)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// deviceVerdict renders the honest behind/last-seen badge: "behind" only when the device
// reported a differing version; otherwise the last-report age (never a "gave up" claim —
// the try-counter is device-NVS-only and not telemetry-observable).
func (p *otaPane) deviceVerdict(d DeviceOTA, now time.Time) string {
	age := "never"
	if d.HasReport && !d.LastSeen.IsZero() {
		age = ageString(d.LastSeen, now)
	}
	if d.Behind {
		return fmt.Sprintf("behind (seen %s)", age)
	}
	return "seen " + age
}

// tableHeading renders a section heading with a focus marker on the active table.
func (p *otaPane) tableHeading(label string, table int) string {
	marker := "  "
	if p.active == table {
		marker = "▼ "
	}
	return lipgloss.NewStyle().Bold(true).Render(marker + label)
}

// renderForm renders the active modal form: title, fields (focused one marked), the
// context line and any validation error.
func (p *otaPane) renderForm(w int) string {
	f := p.form
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(truncate(f.title, w)))
	b.WriteByte('\n')
	if f.scanning {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("  scanning build artifact…"))
		b.WriteByte('\n')
	}
	for i := range f.fields {
		marker := "  "
		if i == f.focus {
			marker = "▸ "
		}
		label := fmt.Sprintf("%-9s", f.fields[i].label+":")
		b.WriteString(truncate(marker+label+" "+f.fields[i].input.View(), w))
		b.WriteByte('\n')
	}
	if f.info != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate("  "+f.info, w)))
		b.WriteByte('\n')
	}
	if f.errMsg != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.err).Render(truncate("  "+f.errMsg, w)))
		b.WriteByte('\n')
	}
	b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("  enter submit · esc cancel · up/down field"))
	return b.String()
}

// renderFooter shows the channel defaults + the load freshness + the write authority.
func (p *otaPane) renderFooter(w int) string {
	now := time.Now()
	chans := make([]string, 0, len(p.state.Channels))
	for _, c := range p.state.Channels {
		def := c.DefaultVersion
		if def == "" {
			def = "—"
		}
		chans = append(chans, c.Name+"="+def)
	}
	authority := "read-only"
	if p.writer != nil {
		if p.directWrite {
			authority = "direct-write"
		} else {
			authority = "admin-api"
		}
	}
	foot := fmt.Sprintf("channels: %s · loaded %s · %s",
		strings.Join(chans, " "), ageString(p.lastLoad, now), authority)
	return lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(foot, w))
}

// renderStatus shows the last action result / non-fatal load error.
func (p *otaPane) renderStatus(w int) string {
	if p.loadErr != nil {
		return lipgloss.NewStyle().Foreground(p.colors.err).Render(truncate("DB error: "+p.loadErr.Error(), w))
	}
	if p.status == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(p.status, w))
}

// ageString renders a coarse relative age (the OTA view never implies a live feed).
func ageString(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// truncate cuts s to a max display width (ANSI-naive; OTA cells are plain text).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
