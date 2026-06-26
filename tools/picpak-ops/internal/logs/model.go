package logs

import (
	"fmt"
	"sort"
	"strings"

	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// logsPane implements the canonical pane.Pane (K1) for the read-only logs axis. All
// mutable fields are touched only on the Bubble Tea message loop (Update / SetFocused /
// SetSize); the store goroutine never touches them — it only delivers addressed messages
// (K2), so the pane needs no lock of its own.
type logsPane struct {
	pane.BasePane

	store  *logStore    // nil ⇒ no database configured (nil pool); the pane renders the no-DB state
	cache  *fleet.Cache // shared serial→label cache (K9); may be nil
	cfg    Config       // render-side snapshot of [logs]
	keys   logsKeys
	colors viewColors
	loc    *time.Location // render timezone (logs.timezone)

	vp     viewport.Model
	follow bool
	filter filterOverlay

	knownSerials []string // distinct serials from the store (filter-picker candidates)
	cur          Cursor   // newest cursor reported by the store (footer)
	backfillDone bool
	err          error
	dirty        bool
}

// logsKeys are the resolved pane-local action keystrokes (Policy=Data, from [logs.keys]).
// An unbound action is "" and never matches.
type logsKeys struct {
	follow       string
	deviceFilter string
	sourceCycle  string
	clearView    string
	copyLines    string
	jumpSerial   string
	reload       string
}

// resolveKeys reads the first chord of each [logs.keys] action (defaults live in the
// config defaults table — no key literal here).
func resolveKeys(m map[string][]string) logsKeys {
	first := func(action string) string {
		if v, ok := m[action]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return logsKeys{
		follow:       first("follow"),
		deviceFilter: first("device_filter"),
		sourceCycle:  first("source_cycle"),
		clearView:    first("clear_view"),
		copyLines:    first("copy"),
		jumpSerial:   first("jump_serial"),
		reload:       first("reload"),
	}
}

// Init starts the store goroutine once (idempotent). The store self-pumps via the Sender,
// so there is no Cmd to return — its first prime arrives as an addressed logRowsMsg. With
// no database (nil store) it does nothing; the no-DB state is static.
func (p *logsPane) Init() tea.Cmd {
	if p.store == nil {
		return nil
	}
	p.store.Start()
	return nil
}

// Update folds the pane's addressed store messages (always delivered, focused or
// backgrounded — K2) and the focused key presses. It never blocks and never queries
// inline (every read is in the store goroutine).
func (p *logsPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {

	case logRowsMsg:
		p.cur = m.cur
		p.err = nil
		p.refresh()
		p.markDirty()
		return p, nil

	case logBackfillMsg:
		p.backfillDone = m.done
		p.refresh()
		return p, nil

	case logSerialsMsg:
		p.knownSerials = m.serials
		return p, nil

	case logErrMsg:
		p.err = m.err
		p.markDirty()
		return p, nil

	case app.SpawnArgsMsg:
		// Optional preselect: another axis (telemetry/flash) may open logs on a serial.
		if s := m.Args["serial"]; s != "" && p.store != nil {
			p.store.SetFilter(Filter{Serials: []string{s}, Sources: p.store.FilterSources()})
		}
		return p, nil

	case app.ConfigChangedMsg:
		if m.Config != nil {
			p.applyConfig(m.Config)
		}
		return p, nil

	case tea.KeyPressMsg:
		return p, p.onKey(m)
	}
	return p, nil
}

// onKey dispatches pane-local actions (reached only while focused; global chrome keys are
// consumed by the shell first, K7). When the device-filter overlay is open it captures
// navigation keys.
func (p *logsPane) onKey(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if p.filter.open {
		p.onFilterKey(key)
		return nil
	}
	switch key {
	case p.keys.follow:
		p.toggleFollow()
		return nil
	case p.keys.deviceFilter:
		p.openFilter()
		return nil
	case p.keys.sourceCycle:
		p.cycleSource()
		return nil
	case p.keys.clearView:
		if p.store != nil {
			p.store.Clear()
		}
		return nil
	case p.keys.copyLines:
		p.copyVisible()
		return nil
	case p.keys.jumpSerial:
		p.jumpSerial()
		return nil
	case p.keys.reload:
		if p.store != nil {
			p.store.Reload()
		}
		return nil
	}
	// Non-configurable scroll navigation over the viewport.
	switch key {
	case "up", "k":
		p.follow = false
		p.vp.ScrollUp(1)
		p.maybeBackfill()
	case "down", "j":
		p.vp.ScrollDown(1)
		if p.vp.AtBottom() {
			p.follow = true
		}
	case "pgup":
		p.follow = false
		p.vp.ScrollUp(p.vp.Height())
		p.maybeBackfill()
	case "pgdown":
		p.vp.ScrollDown(p.vp.Height())
		if p.vp.AtBottom() {
			p.follow = true
		}
	case "home":
		p.follow = false
		p.vp.GotoTop()
		p.maybeBackfill()
	case "end":
		p.vp.GotoBottom()
		p.follow = true
	}
	return nil
}

// onFilterKey handles navigation while the device-multiselect overlay is open.
func (p *logsPane) onFilterKey(key string) {
	switch key {
	case "up", "k":
		p.filter.move(-1)
	case "down", "j":
		p.filter.move(1)
	case " ", "space":
		p.filter.toggle()
	case "enter":
		p.applyFilter()
	case "esc":
		p.filter.close()
	}
}

// toggleFollow flips auto-scroll-to-newest; enabling snaps to the bottom.
func (p *logsPane) toggleFollow() {
	p.follow = !p.follow
	if p.follow {
		p.vp.GotoBottom()
	}
}

// maybeBackfill requests one older page when the viewport reaches the top and there is
// more history to load.
func (p *logsPane) maybeBackfill() {
	if p.store != nil && !p.backfillDone && p.vp.AtTop() {
		p.store.RequestBackfill()
	}
}

// openFilter opens the device multiselect seeded from the current serial filter.
func (p *logsPane) openFilter() {
	current := []string(nil)
	if p.store != nil {
		current = p.store.FilterSerials()
	}
	p.filter.openWith(p.candidateSerials(), current)
}

// applyFilter hands the overlay's checked serials to the store (which reloads) and closes
// the overlay.
func (p *logsPane) applyFilter() {
	chosen := p.filter.chosen()
	p.filter.close()
	if p.store != nil {
		p.store.SetFilter(Filter{Serials: chosen, Sources: p.store.FilterSources()})
	}
}

// cycleSource rotates the source filter (all → each configured source → all).
func (p *logsPane) cycleSource() {
	if p.store == nil {
		return
	}
	next := cycleSingle(p.cfg.DefaultSources, p.store.FilterSources())
	p.store.SetFilter(Filter{Serials: p.store.FilterSerials(), Sources: next})
}

// jumpSerial isolates the next device serial (all → serial[0] → … → all).
func (p *logsPane) jumpSerial() {
	if p.store == nil {
		return
	}
	known := p.candidateSerials()
	sort.Strings(known)
	next := cycleSingle(known, p.store.FilterSerials())
	p.store.SetFilter(Filter{Serials: next, Sources: p.store.FilterSources()})
}

// copyVisible yanks the currently loaded log lines (plain text) to the clipboard,
// surfacing a non-fatal status line. A headless/no-clipboard host fails silently.
func (p *logsPane) copyVisible() {
	if p.store == nil {
		return
	}
	lines := p.store.Snapshot()
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(p.plainLine(ln))
		b.WriteByte('\n')
	}
	_ = clipboard.WriteAll(b.String())
	if send := p.Sender(); send != nil {
		send(app.StatusMsg(fmt.Sprintf("copied %d log line(s)", len(lines))))
	}
}

// candidateSerials is the union of the store's distinct serials and the fleet cache's
// serials (so a device that has not logged yet is still pickable).
func (p *logsPane) candidateSerials() []string {
	set := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !set[s] {
			set[s] = true
			out = append(out, s)
		}
	}
	for _, s := range p.knownSerials {
		add(s)
	}
	if p.cache != nil {
		for _, d := range p.cache.List() {
			add(d.Serial)
		}
	}
	return out
}

// refresh re-renders the store's current ring snapshot into the viewport, following the
// tail while in follow mode. Driven by every store message, focus regain, and resize, so
// a hidden→shown pane is always consistent with the warm ring (the background-tail
// invariant).
func (p *logsPane) refresh() {
	if p.store == nil {
		return
	}
	lines := p.store.Snapshot()
	rendered := make([]string, len(lines))
	for i, ln := range lines {
		rendered[i] = p.renderLine(ln)
	}
	p.vp.SetContentLines(rendered)
	if p.follow {
		p.vp.GotoBottom()
	}
}

// SetFocused records focus, clears the unread marker, and re-syncs the viewport from the
// warm ring (so returning to a backgrounded pane shows the lines accumulated while
// hidden). Cosmetic only — it never starts or stops the store (focus is not I/O).
func (p *logsPane) SetFocused(focused bool) {
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
		p.refresh()
	}
}

// SetSize recomputes the viewport geometry (the footer reserves 2 rows) and repaints.
func (p *logsPane) SetSize(width, height int) {
	p.BasePane.SetSize(width, height)
	w, h := width, height
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	vpH := h - 2
	if vpH < 1 {
		vpH = 1
	}
	p.vp.SetWidth(w)
	p.vp.SetHeight(vpH)
	p.refresh()
}

// Meta reports an honest status: Error on a non-fatal store error (last good ring kept),
// Active while following a live tail, Quiet when paused (scrolled back), else Idle. Title
// carries the line count + filter summary (or the no-DB note).
func (p *logsPane) Meta() pane.PaneMeta {
	status := pane.StatusIdle
	switch {
	case p.store == nil:
		status = pane.StatusIdle
	case p.err != nil:
		status = pane.StatusError
	case p.follow:
		status = pane.StatusActive
	default:
		status = pane.StatusQuiet
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: status,
		Dirty:  p.dirty,
	}
}

func (p *logsPane) title() string {
	if p.store == nil {
		return "logs · no db"
	}
	n := p.store.Len()
	serials := p.store.FilterSerials()
	scope := "all devices"
	switch len(serials) {
	case 0:
		scope = "all devices"
	case 1:
		scope = "1 device"
	default:
		scope = fmt.Sprintf("%d devices", len(serials))
	}
	return fmt.Sprintf("logs · %s · %d line(s)", scope, n)
}

// Close releases the pane. The per-pane ctx (injected via BasePane) is cancelled by the
// registry BEFORE Close, which ends the store goroutine and cancels any in-flight SELECT;
// the shared pool is owned/closed by main, so Close never touches it. Idempotent.
func (p *logsPane) Close() error { return nil }

// applyConfig re-reads the render-side [logs] keys on a hot-reload (K4). The store keeps
// its startup parse/cadence snapshot (K4: long-lived goroutines snapshot at start); a
// separator/cadence change takes effect on the next pane respawn.
func (p *logsPane) applyConfig(c *config.Config) {
	p.cfg = configFrom(c)
	p.keys = resolveKeys(c.Logs.Keys)
	p.colors = resolveColors(c)
	p.loc = resolveLocation(p.cfg.Timezone)
	p.refresh()
}

func (p *logsPane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}
