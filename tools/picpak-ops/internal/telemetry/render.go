package telemetry

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/config"
)

// viewColors are the chrome roles the telemetry view styles with — sourced from the
// config theme map (no color literal here); a missing role resolves to the zero color.
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

// healthGlyph maps a verdict to a glyph + color (BWRY-agnostic chrome roles).
func (p *telemetryPane) healthGlyph(h Health) (string, color.Color) {
	switch h {
	case HealthOK:
		return "●", p.colors.ok
	case HealthLowBatt, HealthBadBoots:
		return "▲", p.colors.warning
	case HealthBrownout:
		return "▼", p.colors.err
	case HealthOffline:
		return "○", p.colors.muted
	default:
		return "·", p.colors.muted
	}
}

// View renders the pane body sized to the last SetSize: no-DB / empty / list+detail,
// plus the two-freshness footer. The layout draws the surrounding border/title.
func (p *telemetryPane) View() string {
	w, h := p.Size()
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}

	if p.repo == nil {
		return p.renderNoDB(w)
	}

	footer := p.renderFooter(w)
	bodyH := h - 2 // footer + a separating blank line
	if bodyH < 1 {
		bodyH = 1
	}

	if len(p.fleetRows) == 0 {
		return p.renderEmpty(w) + "\n\n" + footer
	}

	listW := w / 3
	if listW < 24 {
		listW = min(24, w)
	}
	detailW := w - listW - 1
	if detailW < 1 {
		detailW = 1
	}
	list := p.renderList(listW, bodyH)
	detail := p.renderDetail(detailW, bodyH)
	body := lipgloss.JoinHorizontal(lipgloss.Top, list, " ", detail)
	return body + "\n" + footer
}

// renderNoDB is the explicit "no database configured" state (nil pool / empty DSN). It
// is a static, honest message — never a crash and never an all-n/a grid.
func (p *telemetryPane) renderNoDB(w int) string {
	muted := lipgloss.NewStyle().Foreground(p.colors.muted)
	return strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Render("telemetry — no database configured"),
		muted.Render(truncate("Set [database].dsn to read the telemetry hypertable.", w)),
		muted.Render(truncate("The bench cockpit (build / hosts / flash / console) runs without a DB.", w)),
	}, "\n")
}

// renderEmpty is the explicit "no telemetry yet" state for a connected-but-empty
// hypertable — the current normal case (the field-path push is unwired). Honest, not a
// misleading grid.
func (p *telemetryPane) renderEmpty(w int) string {
	muted := lipgloss.NewStyle().Foreground(p.colors.muted)
	lines := []string{
		lipgloss.NewStyle().Bold(true).Render("telemetry — no telemetry yet"),
		muted.Render(truncate("Devices report only when they fetch a frame (sparse by design):", w)),
		muted.Render(truncate("a row appears here after the next device wake/fetch.", w)),
	}
	if p.pollErr != nil {
		lines = append(lines, p.renderErrBadge(w))
	}
	return strings.Join(lines, "\n")
}

// renderList is the left device list: cursor · health glyph · serial/label · last-seen
// age. Age is the wall-clock age of the device's latest report (telemetry.time).
func (p *telemetryPane) renderList(w, h int) string {
	now := time.Now()
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(truncate("Devices", w)))
	b.WriteByte('\n')

	rows := p.fleetRows
	// Bound the rendered rows to the available height (header + footer accounted by View).
	maxRows := h - 1
	if maxRows < 1 {
		maxRows = 1
	}
	for i, r := range rows {
		if i >= maxRows {
			b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(
				fmt.Sprintf("… %d more", len(rows)-maxRows)))
			break
		}
		health, _ := Verdict(r.Sample(), nil, p.verdictCfg, now)
		glyph, col := p.healthGlyph(health)
		name := r.Serial
		if dev, ok := p.cache.BySerial(r.Serial); ok && dev.Label != "" {
			name = r.Serial + " · " + dev.Label
		}
		cursor := "  "
		if i == p.cursor {
			cursor = "▸ "
		}
		line := fmt.Sprintf("%s%s %s  %s",
			cursor, lipgloss.NewStyle().Foreground(col).Render(glyph),
			name, Age(r.Time, now))
		if i == p.cursor {
			line = lipgloss.NewStyle().Foreground(p.colors.sel).Bold(true).Render(truncate(line, w))
		} else {
			line = truncate(line, w)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderDetail is the right detail card for the selected device: latest values
// (sentinel-aware), the channel-mismatch badge, the opportunistic extra metrics, the
// health verdict + reasons, and the trend sparklines.
func (p *telemetryPane) renderDetail(w, h int) string {
	now := time.Now()
	row, ok := p.selectedFleetRow()
	if !ok {
		return lipgloss.NewStyle().Foreground(p.colors.muted).Render("(no device selected)")
	}

	var b strings.Builder
	title := row.Serial
	assigned := ""
	if dev, ok := p.cache.BySerial(row.Serial); ok {
		if dev.Label != "" {
			title = row.Serial + " · " + dev.Label
		}
		assigned = dev.Channel
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(truncate(title, w)))
	b.WriteByte('\n')

	// Health verdict from the latest full row when available, else the fleet row.
	sample := row.Sample()
	if p.detail != nil && p.detailSerial == row.Serial {
		sample = p.detail.Sample()
	}
	health, reasons := Verdict(sample, p.previousSample(), p.verdictCfg, now)
	glyph, hcol := p.healthGlyph(health)
	b.WriteString(lipgloss.NewStyle().Foreground(hcol).Bold(true).Render(glyph + " " + health.String()))
	b.WriteByte('\n')
	for _, rsn := range reasons {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate("  · "+rsn, w)))
		b.WriteByte('\n')
	}

	// Last-seen age badge (foreground age, never "live").
	b.WriteString(p.field(w, "last report", Age(row.Time, now)))

	// Latest values (sentinel-aware → n/a where not measured).
	b.WriteString(p.field(w, "battery", BattString(row.BattMV, p.cfg.BattUnit)+"  "+PctString(row.BattPct)))
	b.WriteString(p.field(w, "usb", BoolString(row.USB)))
	b.WriteString(p.field(w, "uptime", UptimeString(row.Uptime)))
	b.WriteString(p.field(w, "boot_count", Int64String(row.BootCount)))
	b.WriteString(p.field(w, "bad_boots", IntString(row.BadBoots)))
	b.WriteString(p.field(w, "reset", ResetReasonString(row.ResetReason)))
	b.WriteString(p.field(w, "firmware", orUnknown(row.RunningVer)))

	// Channel: device-claimed vs authoritative; flag a mismatch.
	chanLine := orUnknown(row.DeviceChannel) + " (device-reported)"
	if assigned != "" {
		chanLine = orUnknown(row.DeviceChannel) + " / " + assigned + " (assigned)"
	}
	b.WriteString(p.field(w, "channel", chanLine))
	if badge := ChannelMismatch(row.DeviceChannel, assigned, p.cfg.ChannelMismatchWarn); badge != "" {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.warning).Render(truncate("  ⚠ "+badge, w)))
		b.WriteByte('\n')
	}

	// Opportunistic extra-JSONB metrics (rssi/temp/heap/tx) — n/a when absent.
	if p.detail != nil && p.detailSerial == row.Serial {
		ex := p.detail.Extra
		b.WriteString(p.field(w, "rssi", IntString(ex.RSSI)))
		b.WriteString(p.field(w, "temp", IntString(ex.Temp)))
	}

	// Trend sparklines for the configured plottable metrics.
	if p.detailSerial == row.Serial && len(p.history) > 0 {
		b.WriteByte('\n')
		b.WriteString(lipgloss.NewStyle().Bold(true).Render("trend (oldest → newest)"))
		b.WriteByte('\n')
		for _, metric := range p.cfg.SparklineMetrics {
			series := p.seriesFor(metric)
			if len(series) == 0 {
				continue
			}
			b.WriteString(truncate(fmt.Sprintf("  %-10s %s", metric, sparkline(series)), w))
			b.WriteByte('\n')
		}
	} else if p.detailSerial != row.Serial {
		b.WriteString(lipgloss.NewStyle().Foreground(p.colors.muted).Render("  loading history…"))
		b.WriteByte('\n')
	}

	return b.String()
}

// field renders one "label: value" detail line.
func (p *telemetryPane) field(w int, label, value string) string {
	line := fmt.Sprintf("%-11s %s", label+":", value)
	return truncate(line, w) + "\n"
}

// renderFooter keeps the two freshnesses DISTINCT so neither is mistaken for the other
// (design 08 §4.2): "DB polled <lastPoll>" (when the TUI last queried) vs "newest
// device report <devSeen>" (max(telemetry.time) — when a device actually reported).
func (p *telemetryPane) renderFooter(w int) string {
	now := time.Now()
	parts := []string{
		"DB polled " + Age(p.lastPoll, now),
		"newest device report " + Age(p.devSeen, now),
	}
	foot := strings.Join(parts, " · ")
	if p.pollErr != nil {
		return p.renderErrBadge(w) + "\n" + lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(foot, w))
	}
	return lipgloss.NewStyle().Foreground(p.colors.muted).Render(truncate(foot, w))
}

// renderErrBadge renders the non-fatal DB-error badge (last good data is retained).
func (p *telemetryPane) renderErrBadge(w int) string {
	return lipgloss.NewStyle().Foreground(p.colors.err).Render(truncate("DB error: "+p.pollErr.Error(), w))
}

// selectedFleetRow returns the fleet row under the cursor.
func (p *telemetryPane) selectedFleetRow() (FleetRow, bool) {
	if p.cursor < 0 || p.cursor >= len(p.fleetRows) {
		return FleetRow{}, false
	}
	return p.fleetRows[p.cursor], true
}

// previousSample returns the second-newest history sample for the selected device (the
// uptime-rollback heuristic's prev), or nil when history is unavailable. History is
// time DESC, so index 1 is the row before the latest.
func (p *telemetryPane) previousSample() *Sample {
	if p.detailSerial != p.selected || len(p.history) < 2 {
		return nil
	}
	s := p.history[1].Sample()
	return &s
}

// seriesFor extracts an oldest→newest Null[int] series for a sparkline metric from the
// (time DESC) history. An unknown metric yields an empty series (skipped).
func (p *telemetryPane) seriesFor(metric string) []Null[int] {
	out := make([]Null[int], 0, len(p.history))
	for i := len(p.history) - 1; i >= 0; i-- {
		h := p.history[i]
		switch metric {
		case "batt_pct":
			out = append(out, h.BattPct)
		case "batt_mv":
			out = append(out, h.BattMV)
		case "bad_boots":
			out = append(out, h.BadBoots)
		case "uptime_ms":
			out = append(out, downcast(h.Uptime))
		case "boot_count":
			out = append(out, downcast(h.BootCount))
		default:
			return nil
		}
	}
	return out
}

// sparklineBars is the 8-level block ramp.
var sparklineBars = []rune("▁▂▃▄▅▆▇█")

// sparkline renders a Null[int] series as a unicode block sparkline; an n/a point is a
// gap (space) so a sentinel is never plotted as a spurious zero.
func sparkline(series []Null[int]) string {
	lo, hi, any := 0, 0, false
	for _, n := range series {
		if v, ok := n.Get(); ok {
			if !any {
				lo, hi = v, v
				any = true
			} else {
				if v < lo {
					lo = v
				}
				if v > hi {
					hi = v
				}
			}
		}
	}
	if !any {
		return naLabel
	}
	span := hi - lo
	var b strings.Builder
	for _, n := range series {
		v, ok := n.Get()
		if !ok {
			b.WriteByte(' ')
			continue
		}
		idx := 0
		if span > 0 {
			idx = (v - lo) * (len(sparklineBars) - 1) / span
		}
		b.WriteRune(sparklineBars[idx])
	}
	return b.String()
}

// downcast narrows a Null[int64] to Null[int] for plotting (telemetry int64 metrics fit
// an int on every supported platform; an n/a stays n/a).
func downcast(n Null[int64]) Null[int] {
	if v, ok := n.Get(); ok {
		return some(int(v))
	}
	return Null[int]{}
}

// orUnknown renders an empty string as "unknown" (NULL TEXT and "" render identically).
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// truncate cuts s to a max display width (ANSI-naive; telemetry cells are plain text).
func truncate(s string, w int) string {
	if w <= 1 || lipgloss.Width(s) <= w {
		return s
	}
	if len(s) > w-1 {
		return s[:w-1] + "…"
	}
	return s
}
