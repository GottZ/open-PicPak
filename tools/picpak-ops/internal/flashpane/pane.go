// Package flashpane is the wm-shell view over the flash rollout (W7 / axis 05). It
// implements the canonical pane.Pane (embedding pane.BasePane): idle it is a
// multi-select target picker over the VID-gated discovery inventory
// (HostRegistry.Snapshot); running it is a PER-DEVICE matrix (one row per target,
// never a single batch glyph); done each row shows ✓/✗ + fail class with retry /
// open-console recovery. It reports Meta().Status == StatusWorking while any target is
// non-terminal (K5 quit-guard), and consumes the ONE shared HostRegistry (K9) for the
// picker, the Confirm gate, and the esptool Run.
package flashpane

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/flash"
	"github.com/open-picpak/picpak-ops/internal/pane"
	"github.com/open-picpak/picpak-ops/internal/sshhost"
)

// mode is the pane's top-level view: the idle target picker or the running/done matrix.
type mode int

const (
	modePick mode = iota
	modeMatrix
)

// tickMsg refreshes the idle picker from the live inventory snapshot so a device that
// (dis)appears between discovery polls shows up without a key press.
type tickMsg struct{}

// pickRow is one selectable (host, device) line in the idle picker.
type pickRow struct {
	target flash.Target
}

// flashPane implements pane.Pane for the flash axis.
type flashPane struct {
	pane.BasePane

	cfg    *config.Config
	fc     config.Flash
	reg    *sshhost.HostRegistry
	ctrl   *flash.Controller
	colors viewColors

	mode mode

	// picker state (idle).
	picks    []pickRow
	cursor   int
	selected map[flash.TargetID]bool

	// matrix state (running/done), mutated only on the message loop.
	order       []flash.TargetID
	results     map[flash.TargetID]*flash.DeviceResult
	prog        map[flash.TargetID]*flash.Progress
	runToTarget map[sshhost.RunID]flash.TargetID
	batchDone   bool

	dirty bool
}

// New is the flash-pane factory (registered for pane.KindFlash in main.go). It binds
// the pane to the pre-built shared HostRegistry (the §4.5 promotion) so the picker,
// the Confirm gate, and the esptool Run all ride the ONE transport (K9). It opens no
// transport on spawn — the picker only reads Snapshot().
func New(reg *sshhost.HostRegistry) app.PaneFactory {
	return func(base pane.BasePane, cfg *config.Config) pane.Pane {
		return &flashPane{
			BasePane:    base,
			cfg:         cfg,
			fc:          cfg.Flash,
			reg:         reg,
			colors:      resolveColors(cfg),
			mode:        modePick,
			selected:    map[flash.TargetID]bool{},
			results:     map[flash.TargetID]*flash.DeviceResult{},
			prog:        map[flash.TargetID]*flash.Progress{},
			runToTarget: map[sshhost.RunID]flash.TargetID{},
		}
	}
}

// Init seeds the picker from the current inventory and arms the idle refresh tick.
func (p *flashPane) Init() tea.Cmd {
	p.refreshPicks()
	return tickCmd()
}

// refreshPicks rebuilds the picker rows from the live VID-gated inventory snapshot,
// preserving selection by target id and clamping the cursor.
func (p *flashPane) refreshPicks() {
	snap := p.reg.Snapshot()
	var picks []pickRow
	for _, hs := range snap.Hosts {
		for _, d := range hs.Devices {
			picks = append(picks, pickRow{target: flash.NewTarget(d)})
		}
	}
	p.picks = picks
	if p.cursor >= len(p.picks) {
		p.cursor = max(0, len(p.picks)-1)
	}
}

// Update folds the controller's addressed messages (always delivered, FG or BG — K2)
// and the esptool stream (sshhost.RunOutputMsg, keyed by RunID → target) into the
// matrix, and handles the pane-local keys while focused. It never blocks.
func (p *flashPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {

	case tickMsg:
		if p.mode == modePick {
			p.refreshPicks()
		}
		return p, tickCmd()

	case app.ConfigChangedMsg:
		if m.Config != nil {
			p.cfg = m.Config
			p.fc = m.Config.Flash
			p.colors = resolveColors(m.Config)
		}
		return p, nil

	case flash.FlashStartedMsg:
		// Ensure a matrix row exists for every target (retry adds one without wiping).
		for _, t := range m.Targets {
			if _, ok := p.results[t.ID]; !ok {
				p.order = append(p.order, t.ID)
				p.results[t.ID] = &flash.DeviceResult{Target: t, Phase: flash.PhasePending, NumFiles: len(p.cfg.Build.Artifacts)}
			}
		}
		p.mode = modeMatrix
		p.batchDone = false
		p.markDirty()
		return p, nil

	case flash.FlashPhaseMsg:
		if r := p.results[m.TargetID]; r != nil {
			r.Phase = m.Phase
		}
		p.markDirty()
		return p, nil

	case flash.FlashRunStartedMsg:
		p.runToTarget[m.RunID] = m.TargetID
		p.prog[m.TargetID] = flash.NewProgress(m.NumFiles)
		if r := p.results[m.TargetID]; r != nil {
			r.NumFiles = m.NumFiles
			r.Verified = 0
			r.Pct = 0
		}
		return p, nil

	case sshhost.RunOutputMsg:
		if tid, ok := p.runToTarget[m.RunID]; ok {
			if pr := p.prog[tid]; pr != nil {
				pr.Ingest(m.Line)
				if r := p.results[tid]; r != nil {
					r.Pct = pr.CurrentPct
					r.Verified = pr.Verified
				}
			}
			p.markDirty()
		}
		return p, nil

	case sshhost.RunExitMsg:
		// The controller drives the terminal verdict off run.Done(); the pane ignores
		// the addressed exit so it does not double-classify.
		return p, nil

	case flash.FlashDeviceDoneMsg:
		if r := p.results[m.TargetID]; r != nil {
			r.OK = m.OK
			r.Class = m.Class
			r.Hint = m.Hint
			r.Tail = m.Tail
			if m.OK {
				r.Phase = flash.PhaseDone
			} else {
				r.Phase = flash.PhaseFailed
			}
		}
		p.markDirty()
		return p, nil

	case flash.FlashBatchDoneMsg:
		p.batchDone = true
		p.markDirty()
		return p, nil

	case tea.KeyPressMsg:
		return p, p.onKey(m)
	}
	return p, nil
}

// onKey handles the pane-local bindings (reached only while focused; global chrome
// keys are consumed by the shell first).
func (p *flashPane) onKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	switch s {
	case "up", "k":
		p.moveCursor(-1)
		return nil
	case "down", "j":
		p.moveCursor(1)
		return nil
	case "space", " ":
		if p.mode == modePick {
			p.toggleSelect()
		}
		return nil
	}
	switch s {
	case p.fc.Keys.Run, "enter":
		if p.mode == modePick {
			p.startFlash()
		}
		return nil
	case p.fc.Keys.Abort:
		p.abort()
		return nil
	case p.fc.Keys.Retry:
		p.retryCursor()
		return nil
	case p.fc.Keys.Console:
		p.openConsole()
		return nil
	}
	return nil
}

// moveCursor moves the selection cursor within the active list (picker or matrix).
func (p *flashPane) moveCursor(delta int) {
	n := p.rowCount()
	if n == 0 {
		return
	}
	p.cursor = clamp(p.cursor+delta, 0, n-1)
}

func (p *flashPane) rowCount() int {
	if p.mode == modePick {
		return len(p.picks)
	}
	return len(p.order)
}

// toggleSelect flips the selection of the cursor's picker row.
func (p *flashPane) toggleSelect() {
	if p.cursor < 0 || p.cursor >= len(p.picks) {
		return
	}
	id := p.picks[p.cursor].target.ID
	if p.selected[id] {
		delete(p.selected, id)
	} else {
		p.selected[id] = true
	}
}

// selectedTargets is the chosen target set: the explicitly-selected rows, or the
// cursor row when nothing is selected (so a single 'f' flashes the highlighted device).
func (p *flashPane) selectedTargets() []flash.Target {
	var out []flash.Target
	for _, row := range p.picks {
		if p.selected[row.target.ID] {
			out = append(out, row.target)
		}
	}
	if len(out) == 0 && p.cursor >= 0 && p.cursor < len(p.picks) {
		out = append(out, p.picks[p.cursor].target)
	}
	return out
}

// startFlash builds the controller (once) and starts a fresh batch over the selected
// targets, resetting the matrix.
func (p *flashPane) startFlash() {
	targets := p.selectedTargets()
	if len(targets) == 0 {
		return
	}
	p.ensureController()
	// Fresh matrix.
	p.order = nil
	p.results = map[flash.TargetID]*flash.DeviceResult{}
	p.prog = map[flash.TargetID]*flash.Progress{}
	p.runToTarget = map[sshhost.RunID]flash.TargetID{}
	p.batchDone = false
	p.cursor = 0
	p.mode = modeMatrix
	p.ctrl.Start(targets)
}

// retryCursor re-runs the cursor's failed device through the same controller without
// disturbing the other rows.
func (p *flashPane) retryCursor() {
	if p.mode != modeMatrix || p.cursor < 0 || p.cursor >= len(p.order) {
		return
	}
	id := p.order[p.cursor]
	r := p.results[id]
	if r == nil || r.Phase != flash.PhaseFailed {
		return
	}
	p.ensureController()
	r.Phase = flash.PhasePending
	r.OK = false
	r.Class = flash.FailNone
	r.Hint = ""
	r.Pct = 0
	r.Verified = 0
	p.prog[id] = nil
	p.ctrl.Start([]flash.Target{r.Target})
}

// abort cancels in-flight work: the cursor row's run in the matrix, or all of them.
func (p *flashPane) abort() {
	if p.ctrl == nil {
		return
	}
	if p.mode == modeMatrix && p.cursor >= 0 && p.cursor < len(p.order) {
		id := p.order[p.cursor]
		if r := p.results[id]; r != nil && !r.Phase.Terminal() {
			p.ctrl.Cancel(id)
			return
		}
	}
	p.ctrl.CancelAll()
}

// openConsole spawns an axis-06 console on the cursor device for recovery. It emits a
// top-level SpawnPaneMsg via the Sender (a pane-returned Cmd would be re-addressed
// back to this pane, never reaching the root spawn path).
func (p *flashPane) openConsole() {
	var t flash.Target
	switch {
	case p.mode == modePick && p.cursor < len(p.picks):
		t = p.picks[p.cursor].target
	case p.mode == modeMatrix && p.cursor < len(p.order):
		if r := p.results[p.order[p.cursor]]; r != nil {
			t = r.Target
		}
	default:
		return
	}
	if t.Host == "" || t.Port() == "" {
		return
	}
	if send := p.Sender(); send != nil {
		send(app.SpawnPaneMsg{
			Kind: pane.KindConsole,
			Args: map[string]string{
				"host":   t.Host,
				"port":   t.Port(),
				"serial": t.Device.Serial,
				"label":  t.Device.Label,
			},
			Focus: true,
		})
	}
}

func (p *flashPane) ensureController() {
	if p.ctrl == nil {
		p.ctrl = flash.NewControllerForRegistry(p.Context(), p.cfg, p.reg, p.ID(), p.Sender())
	}
}

// SetFocused clears the unread marker on focus (cosmetic; never opens/closes a
// transport, R5).
func (p *flashPane) SetFocused(focused bool) {
	p.BasePane.SetFocused(focused)
	if focused {
		p.dirty = false
	}
}

// Meta reports StatusWorking while any target row is non-terminal (the K5 quit-guard
// reads this), Error when every row finished with at least one failure, else Idle.
func (p *flashPane) Meta() pane.PaneMeta {
	status := pane.StatusIdle
	if p.mode == modeMatrix {
		anyWorking, anyFail := false, false
		for _, id := range p.order {
			r := p.results[id]
			if r == nil {
				continue
			}
			if !r.Phase.Terminal() {
				anyWorking = true
			}
			if r.Phase == flash.PhaseFailed {
				anyFail = true
			}
		}
		switch {
		case anyWorking:
			status = pane.StatusWorking
		case anyFail:
			status = pane.StatusError
		}
	}
	return pane.PaneMeta{
		ID:     p.ID(),
		Kind:   p.Kind(),
		Title:  p.title(),
		Status: status,
		Dirty:  p.dirty,
	}
}

func (p *flashPane) title() string {
	if p.mode == modeMatrix {
		done, total := 0, len(p.order)
		for _, id := range p.order {
			if r := p.results[id]; r != nil && r.Phase.Terminal() {
				done++
			}
		}
		return "flash · " + itoa(done) + "/" + itoa(total)
	}
	return "flash · " + itoa(len(p.picks)) + " device(s)"
}

// Close cancels all in-flight work (the controller's per-target ctxs, which cancel the
// transport runs). Idempotent.
func (p *flashPane) Close() error {
	if p.ctrl != nil {
		p.ctrl.Close()
	}
	return nil
}

func (p *flashPane) markDirty() {
	if !p.Focused() {
		p.dirty = true
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
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
