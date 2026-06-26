package telemetry

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// baseTick is the pane's own tick granularity (K3). Like the build/flash panes' fixed
// clocks it is an implementation cadence floor, NOT a policy value: the real poll
// intervals are config-driven (poll_interval_focused / poll_interval_background) and
// the pane only ISSUES a poll when one of those has elapsed. A 1s floor makes a
// focus-triggered poll feel immediate without a second timer subsystem.
const baseTick = time.Second

// telemetryPane implements the canonical pane.Pane (K1) for the read-only telemetry
// axis. All mutable fields are touched ONLY on the Bubble Tea message loop (Update /
// SetFocused) — the poll goroutines only return a tea.Msg and never touch pane state,
// so the single-in-flight `polling` guard is race-free without a mutex.
type telemetryPane struct {
	pane.BasePane

	repo        *Repo // nil ⇒ no database configured (nil pool); the pane renders the no-DB state
	cache       *fleet.Cache
	cfg         config.Telemetry
	stmtTimeout time.Duration
	verdictCfg  VerdictCfg
	colors      viewColors
	keys        telemetryKeys

	// snapshot state (loop-owned).
	fleetRows    []FleetRow
	cursor       int
	selected     string // serial under the cursor
	detail       *DeviceTelemetry
	history      []HistoryPoint
	detailSerial string // serial p.detail/p.history belong to ("" = none loaded)

	lastPoll time.Time // when the fleet snapshot was fetched (DB-poll freshness)
	devSeen  time.Time // max(telemetry.time) across the fleet (device-report freshness)
	pollErr  error

	// cadence/guard state (loop-owned).
	polling          bool
	forcePoll        bool      // set by SetFocused(true): poll on the next base tick
	lastPollAt       time.Time // when the last fleet poll was issued
	lastDevicePollAt time.Time // when the last device poll was issued

	dirty bool
}

// telemetryKeys are the resolved pane-local action keystrokes (Policy=Data, from
// [telemetry.keys]). An unbound action is "" and simply never matches.
type telemetryKeys struct {
	refresh    string
	next       string
	prev       string
	copySerial string
}

// resolveKeys reads the first chord of each [telemetry.keys] action. Defaults live in
// the config defaults table (no magic keystroke literal here); an absent action stays
// "".
func resolveKeys(m map[string][]string) telemetryKeys {
	first := func(action string) string {
		if v, ok := m[action]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return telemetryKeys{
		refresh:    first("refresh"),
		next:       first("select_next"),
		prev:       first("select_prev"),
		copySerial: first("copy_serial"),
	}
}

// New is the telemetry-pane factory (registered for pane.KindTelemetry in main.go). It
// closes over the shared (possibly nil) read pool and the shared fleet cache (K9). A
// nil pool ⇒ no Repo ⇒ the pane renders "no database configured" and never polls —
// the bench cockpit (Phase C) runs without a DB.
func New(pool *pgxpool.Pool, cache *fleet.Cache) app.PaneFactory {
	return func(base pane.BasePane, cfg *config.Config) pane.Pane {
		p := &telemetryPane{
			BasePane:    base,
			cache:       cache,
			cfg:         cfg.Telemetry,
			stmtTimeout: cfg.Database.StatementTimeout.D(),
			verdictCfg:  VerdictCfgFrom(cfg.Telemetry),
			colors:      resolveColors(cfg),
			keys:        resolveKeys(cfg.Telemetry.Keys),
		}
		if pool != nil {
			p.repo = NewRepo(pool, p.stmtTimeout)
		}
		return p
	}
}

// Init issues the first fleet poll and arms the cadence tick. With no database it does
// nothing (the no-DB state is static).
func (p *telemetryPane) Init() tea.Cmd {
	if p.repo == nil {
		return nil
	}
	return tea.Batch(p.issueFleetPoll(), tickCmd())
}

// Update folds the pane's addressed messages (always delivered, FG or BG — K2) and the
// focused key presses. It never blocks and never queries inline (every read is a Cmd
// goroutine emitting a tea.Msg).
func (p *telemetryPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {

	case tickMsg:
		return p, p.onTick()

	case fleetMsg:
		p.polling = false
		p.fleetRows = m.rows
		p.lastPoll = m.at
		p.devSeen = newestReport(m.rows)
		p.pollErr = nil
		p.syncSelected()
		p.markDirty()
		return p, p.maybePollDevice()

	case deviceMsg:
		p.polling = false
		if m.serial == p.selected {
			p.detail = m.latest
			p.history = m.history
			p.detailSerial = m.serial
		}
		p.pollErr = nil
		p.markDirty()
		return p, p.maybePollDevice()

	case pollErrMsg:
		p.polling = false
		p.pollErr = m.err
		p.markDirty()
		return p, nil

	case app.SpawnArgsMsg:
		// Optional preselect: another axis (flash) may open telemetry on a serial.
		if s := m.Args["serial"]; s != "" {
			p.selected = s
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

// onTick re-arms the base tick and issues a fleet poll when the focused/background
// cadence has elapsed (or a focus-triggered poll is pending) and none is in flight.
func (p *telemetryPane) onTick() tea.Cmd {
	if p.repo == nil {
		return nil
	}
	cmds := []tea.Cmd{tickCmd()} // always re-arm
	due := p.forcePoll || p.lastPollAt.IsZero() || time.Since(p.lastPollAt) >= p.currentInterval()
	if due && !p.polling {
		cmds = append(cmds, p.issueFleetPoll())
	}
	return tea.Batch(cmds...)
}

// currentInterval is the focused vs background poll cadence (Policy=Data). The focused
// cadence reuses telemetry.refresh_interval (reconciled = poll_interval_focused);
// blurred uses poll_interval_background. Being hidden never stops refresh — only the
// cadence drops.
func (p *telemetryPane) currentInterval() time.Duration {
	if p.Focused() {
		if d := p.cfg.RefreshInterval.D(); d > 0 {
			return d
		}
	}
	if d := p.cfg.PollIntervalBackground.D(); d > 0 {
		return d
	}
	return time.Minute
}

// issueFleetPoll marks the in-flight guard, records the issue time, clears the
// focus-poll request, and returns the poll Cmd.
func (p *telemetryPane) issueFleetPoll() tea.Cmd {
	p.polling = true
	p.lastPollAt = time.Now()
	p.forcePoll = false
	return p.pollFleetCmd()
}

// pollFleetCmd runs the fleet snapshot read in a goroutine (Bubble Tea runs the Cmd off
// the loop). It captures the per-pane ctx + repo at issue time; the repo derives the
// statement-timeout-bounded child ctx, so Close()/quit cancels the in-flight SELECT.
func (p *telemetryPane) pollFleetCmd() tea.Cmd {
	ctx := p.Context()
	repo := p.repo
	max := p.cfg.FleetMaxDevices
	return func() tea.Msg {
		rows, err := repo.LatestFleet(ctx, max)
		if err != nil {
			return pollErrMsg{err: err, at: time.Now()}
		}
		return fleetMsg{rows: rows, at: time.Now()}
	}
}

// maybePollDevice issues a device detail+history poll for the selected serial when one
// is needed (selection changed → no detail yet) or stale (cadence elapsed), and none is
// in flight. The "no detail yet" guard breaks the deviceMsg→poll→deviceMsg loop: after
// a fetch, detailSerial==selected and lastDevicePollAt is fresh, so the next call is a
// no-op until the selection changes or the cadence elapses.
func (p *telemetryPane) maybePollDevice() tea.Cmd {
	if p.repo == nil || p.polling || p.selected == "" {
		return nil
	}
	needNew := p.detailSerial != p.selected
	stale := !p.lastDevicePollAt.IsZero() && time.Since(p.lastDevicePollAt) >= p.currentInterval()
	if !needNew && !stale {
		return nil
	}
	p.polling = true
	p.lastDevicePollAt = time.Now()
	return p.pollDeviceCmd(p.selected)
}

// pollDeviceCmd runs the per-device latest + history read in a goroutine.
func (p *telemetryPane) pollDeviceCmd(serial string) tea.Cmd {
	ctx := p.Context()
	repo := p.repo
	window := p.cfg.Window.D()
	max := p.cfg.HistoryMaxRows
	return func() tea.Msg {
		latest, err := repo.LatestDevice(ctx, serial)
		if err != nil {
			return pollErrMsg{err: err, at: time.Now()}
		}
		hist, err := repo.DeviceHistory(ctx, serial, window, max)
		if err != nil {
			return pollErrMsg{err: err, at: time.Now()}
		}
		return deviceMsg{serial: serial, latest: latest, history: hist, at: time.Now()}
	}
}

// onKey handles the pane-local action keystrokes (reached only while focused; global
// chrome keys are consumed by the shell first).
func (p *telemetryPane) onKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case p.keys.next, "down":
		return p.moveCursor(1)
	case p.keys.prev, "up":
		return p.moveCursor(-1)
	case p.keys.refresh:
		if p.repo != nil && !p.polling {
			return p.issueFleetPoll()
		}
		return nil
	case p.keys.copySerial:
		p.copySelectedSerial()
		return nil
	}
	return nil
}

// moveCursor moves the device-list cursor, updates the selected serial, and (if free)
// issues an immediate device poll so selection feels instant on the sparse table.
func (p *telemetryPane) moveCursor(delta int) tea.Cmd {
	if len(p.fleetRows) == 0 {
		return nil
	}
	p.cursor = clamp(p.cursor+delta, 0, len(p.fleetRows)-1)
	p.selected = p.fleetRows[p.cursor].Serial
	return p.maybePollDevice()
}

// copySelectedSerial yanks the selected serial to the clipboard for hand-off to the
// OTA/console axis, surfacing a non-fatal status line. A headless/no-clipboard host
// fails silently — the status line still confirms the action.
func (p *telemetryPane) copySelectedSerial() {
	if p.selected == "" {
		return
	}
	_ = clipboard.WriteAll(p.selected)
	if send := p.Sender(); send != nil {
		send(app.StatusMsg("copied serial " + p.selected))
	}
}

// syncSelected keeps the cursor/selection consistent after a fresh snapshot: it keeps
// the previously-selected serial under the cursor if it still exists, else selects the
// clamped cursor row.
func (p *telemetryPane) syncSelected() {
	if len(p.fleetRows) == 0 {
		return
	}
	if p.selected != "" {
		for i, r := range p.fleetRows {
			if r.Serial == p.selected {
				p.cursor = i
				return
			}
		}
	}
	p.cursor = clamp(p.cursor, 0, len(p.fleetRows)-1)
	p.selected = p.fleetRows[p.cursor].Serial
}

// SetFocused records focus and clears the unread marker. A blurred→focused transition
// requests a poll on the next base tick: the canonical SetFocused(bool) cannot return a
// Cmd (K1), so the immediate refresh is realized as "due on the next ≤1s tick" rather
// than an inline Cmd — close enough that the operator never stares at stale data.
func (p *telemetryPane) SetFocused(focused bool) {
	was := p.Focused()
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
		if !was {
			p.forcePoll = true
		}
	}
}

// Meta reports an honest status: Working only while a poll is in flight (the poll-only
// pane is never "live"), Error on a non-fatal poll error (last data retained), else
// Idle. Title carries the device count (or the no-DB note).
func (p *telemetryPane) Meta() pane.PaneMeta {
	status := pane.StatusIdle
	switch {
	case p.polling:
		status = pane.StatusWorking
	case p.pollErr != nil:
		status = pane.StatusError
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: status,
		Dirty:  p.dirty,
	}
}

func (p *telemetryPane) title() string {
	if p.repo == nil {
		return "telemetry · no db"
	}
	return fmt.Sprintf("telemetry · %d device(s)", len(p.fleetRows))
}

// Close drops the leased state. The per-pane ctx (injected via BasePane) is cancelled
// by the registry BEFORE Close, which cancels any in-flight poll's derived ctx; the
// shared pool is owned/closed by main, so Close never calls pool.Close(). Idempotent.
func (p *telemetryPane) Close() error {
	p.polling = false
	return nil
}

// applyConfig re-reads the [telemetry]/[database] keys on a hot-reload (K4).
func (p *telemetryPane) applyConfig(c *config.Config) {
	p.cfg = c.Telemetry
	p.stmtTimeout = c.Database.StatementTimeout.D()
	p.verdictCfg = VerdictCfgFrom(c.Telemetry)
	p.colors = resolveColors(c)
	p.keys = resolveKeys(c.Telemetry.Keys)
	if p.repo != nil {
		p.repo.statementTimeout = p.stmtTimeout
	}
}

func (p *telemetryPane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}

// tickCmd schedules the next base cadence tick.
func tickCmd() tea.Cmd {
	return tea.Tick(baseTick, func(time.Time) tea.Msg { return tickMsg{} })
}

// newestReport returns max(telemetry.time) across a snapshot (device-report freshness,
// distinct from poll freshness). Zero when the snapshot is empty.
func newestReport(rows []FleetRow) time.Time {
	var newest time.Time
	for _, r := range rows {
		if r.Time.After(newest) {
			newest = r.Time
		}
	}
	return newest
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
