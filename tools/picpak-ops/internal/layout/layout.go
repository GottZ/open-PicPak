package layout

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// Theme is the resolved set of chrome colors + status glyphs the layout draws
// with. wm-shell builds it from the config theme-role map (no color literal in
// this package); a missing role resolves to the zero color (no styling) so the
// engine never bakes a default ANSI index of its own.
type Theme struct {
	Border       color.Color
	FocusBorder  color.Color
	TabActive    color.Color
	TabInactive  color.Color
	StatusActive color.Color
	StatusWork   color.Color
	StatusQuiet  color.Color
	StatusError  color.Color
	Dirty        color.Color

	// Glyphs are the single-rune status markers. They are runtime values (passed
	// in by the shell from config-derivable strings), not literals here.
	GlyphActive  string
	GlyphWorking string
	GlyphQuiet   string
	GlyphIdle    string
	GlyphError   string
	DirtyMarker  string
}

// Options controls fixed chrome placement (from ui.* config). Tabs-first.
type Options struct {
	TabBar    string // "top" | "bottom" | "off"
	StatusBar bool
}

// Model is the layout engine. It is held by pointer by the shell so resize/focus
// mutations are visible without copy-back (wm-shell §4.2). It owns the Tabs and
// the last computed rectangles the hit-test reads.
type Model struct {
	tabs  *Tabs
	theme Theme
	opts  Options

	width, height int

	// rects is recomputed on every relayout; the hit-test reads it. It maps the
	// terminal geometry into named zones.
	rects rects
}

// rects holds the pixel/cell rectangles of the chrome zones for the current size.
type rects struct {
	tabBarRow int  // terminal row of the tab bar (-1 if off)
	tabBarTop bool // tab bar at top (vs bottom)
	bodyTop   int  // first body row (inclusive)
	bodyLeft  int  // first body col (inclusive)
	bodyW     int  // body width  (interior, inside any border)
	bodyH     int  // body height (interior, inside any border)

	// tabSpans maps the tab bar's clickable label ranges to pane ids. Each span is
	// [startCol,endCol) on the tab bar row.
	tabSpans []tabSpan
}

type tabSpan struct {
	id    pane.PaneID
	start int
	end   int
}

// New builds the layout model.
func New(tabs *Tabs, theme Theme, opts Options) *Model {
	return &Model{tabs: tabs, theme: theme, opts: opts, rects: rects{tabBarRow: -1}}
}

// Tabs exposes the tab model for the shell's tab/focus operations.
func (m *Model) Tabs() *Tabs { return m.tabs }

// SetTheme swaps the theme (config hot-reload).
func (m *Model) SetTheme(t Theme) { m.theme = t }

// SetOptions swaps the chrome options (config hot-reload).
func (m *Model) SetOptions(o Options) { m.opts = o }

// SetSize records the terminal size and recomputes the chrome rectangles. It
// returns the interior body size every pane should be sized to.
func (m *Model) SetSize(width, height int) (bodyW, bodyH int) {
	m.width, m.height = width, height
	m.recompute()
	return m.rects.bodyW, m.rects.bodyH
}

// BodySize returns the last computed interior body size.
func (m *Model) BodySize() (w, h int) { return m.rects.bodyW, m.rects.bodyH }

// recompute lays out the tab bar, status line and body region for the current
// size. Body is bordered (1 cell each side); the interior is what panes render
// into. The tab spans are computed here so the hit-test and the render agree.
func (m *Model) recompute() {
	r := rects{tabBarRow: -1}

	tabBar := m.opts.TabBar != "off" && m.tabs.Len() > 0
	statusRows := 0
	if m.opts.StatusBar {
		statusRows = 1
	}
	tabRows := 0
	if tabBar {
		tabRows = 1
	}

	// Vertical budget: [tab bar] + [body] + [status]. The body is the remainder.
	bodyRows := m.height - tabRows - statusRows
	if bodyRows < 0 {
		bodyRows = 0
	}

	if tabBar {
		if m.opts.TabBar == "bottom" {
			// body first, then tab bar above the status line
			r.tabBarRow = tabRows + bodyRows // row just below the body
			r.tabBarTop = false
			r.bodyTop = 0
		} else {
			r.tabBarRow = 0
			r.tabBarTop = true
			r.bodyTop = tabRows
		}
	} else {
		r.bodyTop = 0
	}

	// The body region is bordered; interior is 2 cells smaller each axis.
	r.bodyLeft = 0
	interiorW := m.width - 2
	interiorH := bodyRows - 2
	if interiorW < 0 {
		interiorW = 0
	}
	if interiorH < 0 {
		interiorH = 0
	}
	r.bodyW = interiorW
	r.bodyH = interiorH

	// Tab spans are computed against live pane meta by Refresh (called once per
	// frame and before any mouse routing), not here — recompute has no pane meta.
	m.rects = r
}

// Zone names a hit-test region.
type Zone int

const (
	ZoneNone Zone = iota // click outside any interactive region
	ZoneTab              // click on the tab bar (switch tab)
	ZoneBody             // click inside the active pane body
)

// Hit is the hit-test result.
type Hit struct {
	Zone           Zone
	ID             pane.PaneID // the pane the click maps to ("" if none)
	LocalX, LocalY int         // pane-local coords (valid only for ZoneBody)
}

// HitTest is the single place coordinate→pane math lives (R4). It maps an
// absolute terminal cell (x,y) to a zone and, for the body, to pane-local coords
// inside the active pane's interior. Out-of-range or empty layouts return ZoneNone.
func (m *Model) HitTest(x, y int) Hit {
	r := m.rects
	if x < 0 || y < 0 || x >= m.width || y >= m.height {
		return Hit{Zone: ZoneNone}
	}

	// Tab bar row.
	if r.tabBarRow >= 0 && y == r.tabBarRow {
		for _, s := range r.tabSpans {
			if x >= s.start && x < s.end {
				return Hit{Zone: ZoneTab, ID: s.id}
			}
		}
		return Hit{Zone: ZoneNone}
	}

	// Body interior (inside the 1-cell border). The active/focused pane owns it.
	id, ok := m.tabs.Focused()
	if !ok {
		return Hit{Zone: ZoneNone}
	}
	interiorTop := r.bodyTop + 1
	interiorLeft := r.bodyLeft + 1
	if x >= interiorLeft && x < interiorLeft+r.bodyW &&
		y >= interiorTop && y < interiorTop+r.bodyH {
		return Hit{Zone: ZoneBody, ID: id, LocalX: x - interiorLeft, LocalY: y - interiorTop}
	}
	return Hit{Zone: ZoneNone}
}

// Compose renders the full chrome for the current size: tab bar + bordered active
// pane body + status line. panes maps every active pane id to its Pane (for meta
// + body); the focused pane is drawn in the body. status is the shell status line
// text. The result is a single string the root wraps in its tea.View.
func (m *Model) Compose(panes map[pane.PaneID]pane.Pane, status string) string {
	var top, body, bottom string

	if m.opts.TabBar != "off" && m.tabs.Len() > 0 {
		bar := m.renderTabBar(panes)
		if m.rects.tabBarTop {
			top = bar
		} else {
			bottom = bar
		}
	}

	body = m.renderBody(panes)

	var statusLine string
	if m.opts.StatusBar {
		statusLine = m.renderStatus(status)
	}

	parts := make([]string, 0, 4)
	if top != "" {
		parts = append(parts, top)
	}
	parts = append(parts, body)
	if bottom != "" {
		parts = append(parts, bottom)
	}
	if statusLine != "" {
		parts = append(parts, statusLine)
	}
	return strings.Join(parts, "\n")
}

// renderBody draws the focused pane inside a border whose color depends on focus.
// When there is no pane (launcher edge case handled by the shell), it draws an
// empty bordered box so the chrome stays stable.
func (m *Model) renderBody(panes map[pane.PaneID]pane.Pane) string {
	id, ok := m.tabs.Focused()
	content := ""
	if ok {
		if p, found := panes[id]; found {
			content = p.View()
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(m.theme.FocusBorder).
		Width(m.rects.bodyW).
		Height(m.rects.bodyH)
	return box.Render(content)
}

// renderTabBar draws each tab as "[N glyph title*]" with active/inactive coloring.
// Spans were computed in recompute so this and the hit-test agree on column math.
func (m *Model) renderTabBar(panes map[pane.PaneID]pane.Pane) string {
	var b strings.Builder
	focusedIdx := m.tabs.FocusedIndex()
	for i, id := range m.tabs.Order() {
		label := m.tabLabel(i, panes[id])
		style := lipgloss.NewStyle().Foreground(m.theme.TabInactive)
		if i == focusedIdx {
			style = lipgloss.NewStyle().Foreground(m.theme.TabActive).Bold(true)
		}
		b.WriteString(style.Render(label))
	}
	return b.String()
}

// tabLabel formats one tab label (number, status glyph, title, dirty marker). The
// status glyph is colored by its themed status role and the dirty marker by the
// dirty role; all glyphs/colors come from the theme (config-derived), never literal
// here. lipgloss.Width strips the embedded ANSI, so span/hit-test math is unaffected.
func (m *Model) tabLabel(idx int, p pane.Pane) string {
	title := ""
	glyph := lipgloss.NewStyle().Render(m.theme.GlyphIdle)
	dirty := ""
	if p != nil {
		meta := p.Meta()
		title = meta.Title
		if title == "" {
			title = string(meta.Kind)
		}
		glyph = m.styledGlyph(meta.Status)
		if meta.Dirty {
			dirty = lipgloss.NewStyle().Foreground(m.theme.Dirty).Render(m.theme.DirtyMarker)
		}
	}
	// 1-based number for alt+N parity with the focus-by-number keymap.
	return fmt.Sprintf(" %d %s %s%s ", idx+1, glyph, title, dirty)
}

// styledGlyph returns the themed status glyph rendered in its themed status color.
func (m *Model) styledGlyph(s pane.PaneStatus) string {
	var glyph string
	var c color.Color
	switch s {
	case pane.StatusActive:
		glyph, c = m.theme.GlyphActive, m.theme.StatusActive
	case pane.StatusWorking:
		glyph, c = m.theme.GlyphWorking, m.theme.StatusWork
	case pane.StatusQuiet:
		glyph, c = m.theme.GlyphQuiet, m.theme.StatusQuiet
	case pane.StatusError:
		glyph, c = m.theme.GlyphError, m.theme.StatusError
	default:
		glyph, c = m.theme.GlyphIdle, m.theme.TabInactive
	}
	return lipgloss.NewStyle().Foreground(c).Render(glyph)
}

// renderStatus draws the status line, truncated/padded to the terminal width.
func (m *Model) renderStatus(status string) string {
	style := lipgloss.NewStyle().Foreground(m.theme.TabInactive).Width(m.width)
	return style.Render(status)
}

// Refresh recomputes tab spans against the LIVE pane meta so the hit-test column
// math matches exactly what renderTabBar drew. The shell calls this once per frame
// after pane meta may have changed (titles/dirty/status), before any mouse routing.
func (m *Model) Refresh(panes map[pane.PaneID]pane.Pane) {
	if m.opts.TabBar == "off" || m.tabs.Len() == 0 {
		m.rects.tabSpans = nil
		return
	}
	spans := make([]tabSpan, 0, m.tabs.Len())
	col := 0
	for i, id := range m.tabs.Order() {
		label := m.tabLabel(i, panes[id])
		w := lipgloss.Width(label)
		spans = append(spans, tabSpan{id: id, start: col, end: col + w})
		col += w
	}
	m.rects.tabSpans = spans
}
