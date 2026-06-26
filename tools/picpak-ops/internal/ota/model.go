package ota

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/telemetry"
)

// baseTick is the pane's own tick granularity (K3): a 1s floor that makes a focus- or
// write-triggered refresh feel immediate; the real re-poll cadence is the config
// ota.refresh_interval, counted off this floor.
const baseTick = time.Second

// fallbackFleetCap bounds the running-version fetch when no telemetry fleet cap is
// configured (a safety cap, not a tunable).
const fallbackFleetCap = 1000

// activeTable selects which table owns the cursor.
const (
	tableVersions = 0
	tableDevices  = 1
)

// otaPane implements the canonical pane.Pane (K1) for the OTA-management axis. All
// mutable fields are touched ONLY on the Bubble Tea message loop (Update / SetFocused /
// SetSize); the read/write goroutines never touch pane state — they only return an
// addressed message (K2), so the single-in-flight `inFlight` guard is race-free without
// a mutex.
type otaPane struct {
	pane.BasePane

	store  *Store          // nil ⇒ no database configured (nil pool); pane renders the no-DB state
	repo   *telemetry.Repo // nil ⇒ no running_ver source (target-vs-running shows "?")
	cache  *fleet.Cache    // shared serial→label/channel cache (K9); may be nil
	writer Writer          // nil ⇒ writes disabled (read-only mode)

	cfg         *config.Config // full config (ScanArtifact + reload); read-only snapshot
	stmtTimeout time.Duration
	fleetCap    int
	precedence  []string
	directWrite bool
	colors      viewColors
	keys        otaKeys

	// snapshot state (loop-owned).
	state    OTAState
	verCur   int
	devCur   int
	active   int // tableVersions | tableDevices
	lastLoad time.Time
	loadErr  error

	// form sub-state (nil unless registering/pinning/setting a channel).
	form *formState

	// cadence/guard state (loop-owned).
	inFlight   bool
	forceLoad  bool      // set by SetFocused(true): reload on the next base tick
	lastLoadAt time.Time // when the last load was issued

	pendingWrite bool // a write completed while blurred → Meta().Dirty badge
	dirty        bool // unseen state change while blurred
	status       string
}

// otaKeys are the resolved pane-local action keystrokes (Policy=Data, from [ota.keys]).
// An unbound action is "" and never matches.
type otaKeys struct {
	register    string
	pin         string
	unpin       string
	setChannel  string
	toggleState string
	markDone    string
	refresh     string
	switchTable string
}

func resolveKeys(m map[string][]string) otaKeys {
	first := func(action string) string {
		if v, ok := m[action]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return otaKeys{
		register:    first("register"),
		pin:         first("pin"),
		unpin:       first("unpin"),
		setChannel:  first("set_channel"),
		toggleState: first("toggle_state"),
		markDone:    first("mark_done"),
		refresh:     first("refresh"),
		switchTable: first("switch_table"),
	}
}

// New is the OTA-pane factory (registered for pane.KindOTA in main.go). It closes over
// the shared (possibly nil) read pool, the telemetry Repo (for running_ver — consuming
// LatestFleet, NOT a duplicated latest-row query), and the shared fleet cache (K9). A
// nil pool ⇒ no Store ⇒ the pane renders "no database configured" and never reads/writes.
// The Writer backend is chosen by ota.allow_direct_write (NewWriter); a construction
// error (e.g. direct-write flag with no pool) is surfaced into the status line, not a
// crash.
func New(pool *pgxpool.Pool, repo *telemetry.Repo, cache *fleet.Cache) app.PaneFactory {
	return func(base pane.BasePane, cfg *config.Config) pane.Pane {
		p := &otaPane{
			BasePane:    base,
			repo:        repo,
			cache:       cache,
			cfg:         cfg,
			stmtTimeout: cfg.Database.StatementTimeout.D(),
			fleetCap:    fleetCapFrom(cfg),
			precedence:  append([]string(nil), cfg.OTA.ResolvePrecedence...),
			colors:      resolveColors(cfg),
			keys:        resolveKeys(cfg.OTA.Keys),
			active:      tableDevices,
		}
		if pool != nil {
			p.store = NewStore(pool, p.stmtTimeout)
		}
		w, err := NewWriter(cfg, pool)
		if err != nil {
			p.status = err.Error()
		}
		p.writer = w
		if w != nil {
			p.directWrite = w.IsDirect()
		}
		return p
	}
}

// fleetCapFrom reuses the telemetry fleet cap (one config concept) when set, else a
// defensive fallback — never a magic literal scattered in code.
func fleetCapFrom(cfg *config.Config) int {
	if n := cfg.Telemetry.FleetMaxDevices; n > 0 {
		return n
	}
	return fallbackFleetCap
}

// Init issues the first load and arms the cadence tick. With no database it does nothing
// (the no-DB state is static).
func (p *otaPane) Init() tea.Cmd {
	if p.store == nil {
		return nil
	}
	return tea.Batch(p.issueLoad(), tickCmd())
}

// issueLoad sets the in-flight guard, records the issue time, clears the force-load
// request, and returns the load Cmd.
func (p *otaPane) issueLoad() tea.Cmd {
	p.inFlight = true
	p.lastLoadAt = time.Now()
	p.forceLoad = false
	return p.loadCmd()
}

// currentInterval is the refresh cadence (Policy=Data: ota.refresh_interval). Being
// hidden never stops refresh — the tick keeps counting; only the eventual cadence is the
// same configured interval (no separate background cadence key in [ota]).
func (p *otaPane) currentInterval() time.Duration {
	if d := p.cfg.OTA.RefreshInterval.D(); d > 0 {
		return d
	}
	return 30 * time.Second
}

// SetFocused records focus, clears the unread + pending-write markers, and requests a
// reload on the next base tick (K1 SetFocused cannot return a Cmd, so the immediate
// refresh is realized as "due on the next ≤1s tick"). On blur it blurs any open form
// field so a backgrounded pane cannot keep a text cursor armed.
func (p *otaPane) SetFocused(focused bool) {
	was := p.Focused()
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
		p.pendingWrite = false
		if !was {
			p.forceLoad = true
		}
		p.focusFormField()
		return
	}
	p.blurFormField()
}

// Meta reports an honest status: Working while a load/write is in flight, Error on a
// non-fatal load error (last data retained), else Idle. Dirty is the unseen-change OR
// pending-write badge (the `*` tab marker). Title carries the version count / no-DB note.
func (p *otaPane) Meta() pane.PaneMeta {
	status := pane.StatusIdle
	switch {
	case p.inFlight:
		status = pane.StatusWorking
	case p.loadErr != nil:
		status = pane.StatusError
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: status,
		Dirty:  p.dirty || p.pendingWrite,
	}
}

func (p *otaPane) title() string {
	if p.store == nil {
		return "ota · no db"
	}
	if p.directWrite {
		return fmt.Sprintf("ota · %d ver · DIRECT-WRITE", len(p.state.Versions))
	}
	return fmt.Sprintf("ota · %d version(s)", len(p.state.Versions))
}

// Close drops the leased state. The per-pane ctx is cancelled by the registry BEFORE
// Close (cancelling any in-flight read/write's derived ctx); the shared pool is
// owned/closed by main, so Close never calls pool.Close(). Idempotent.
func (p *otaPane) Close() error {
	p.inFlight = false
	if p.writer != nil {
		return p.writer.Close()
	}
	return nil
}

// applyConfig re-reads the [ota]/[database] keys on a hot-reload (K4). It rebuilds the
// writer so a flipped allow_direct_write / changed admin URL takes effect.
func (p *otaPane) applyConfig(c *config.Config, pool *pgxpool.Pool) {
	p.cfg = c
	p.stmtTimeout = c.Database.StatementTimeout.D()
	p.fleetCap = fleetCapFrom(c)
	p.precedence = append([]string(nil), c.OTA.ResolvePrecedence...)
	p.colors = resolveColors(c)
	p.keys = resolveKeys(c.OTA.Keys)
	if p.store != nil {
		p.store.statementTimeout = p.stmtTimeout
	}
}

func (p *otaPane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}

// tickCmd schedules the next base cadence tick.
func tickCmd() tea.Cmd {
	return tea.Tick(baseTick, func(time.Time) tea.Msg { return tickMsg{} })
}

// newInput builds a textinput pre-themed for the forms: single-line, label rendered by
// the view (no prompt), a virtual cursor so it shows without a blink Cmd (the console
// pane's pattern), and no global-key capture (the shell resolves chrome keys first).
func newInput(placeholder, value string) textinput.Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = placeholder
	in.SetVirtualCursor(true)
	in.SetValue(value)
	return in
}
