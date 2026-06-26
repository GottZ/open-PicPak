package ota

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// formKind names the modal sub-state forms (register / pin / set-channel).
type formKind int

const (
	formRegister formKind = iota
	formPin
	formChannel
)

// formField is one labeled text input in a form.
type formField struct {
	label string
	input textinput.Model
}

// formState is the modal form overlay (Bubbles textinput per field). It is constrained
// to text entry + esc/enter + up/down navigation (the shell resolves global chrome keys
// at dispatch step 2 BEFORE the pane sees them, so a form cannot swallow a global chord;
// design 07 form key-capture caveat).
type formState struct {
	kind     formKind
	title    string
	info     string // context line (e.g. the scanned sha/size)
	fields   []formField
	focus    int
	art      *FirmwareArtifact // register: the scanned artifact
	errMsg   string
	scanning bool
}

func (f *formState) value(label string) string {
	for i := range f.fields {
		if f.fields[i].label == label {
			return f.fields[i].input.Value()
		}
	}
	return ""
}

// Update folds the pane's addressed messages (always delivered, FG or BG — K2) and the
// focused key presses. It never blocks and never queries inline (every read/write is a
// Cmd goroutine emitting a tea.Msg).
func (p *otaPane) Update(msg tea.Msg) (pane.Pane, tea.Cmd) {
	switch m := msg.(type) {
	case tickMsg:
		return p, p.onTick()

	case otaStateMsg:
		p.inFlight = false
		p.state = m.state
		p.lastLoad = m.at
		p.loadErr = nil
		p.clampCursors()
		p.markDirty()
		return p, nil

	case otaErrMsg:
		p.inFlight = false
		p.loadErr = m.err
		p.markDirty()
		return p, nil

	case fwArtifactScannedMsg:
		return p, p.onArtifactScanned(m)

	case writeResultMsg:
		return p, p.onWriteResult(m)

	case app.SpawnArgsMsg:
		if s := m.Args["serial"]; s != "" {
			p.selectDeviceBySerial(s)
		}
		return p, nil

	case app.ConfigChangedMsg:
		if m.Config != nil {
			p.applyConfig(m.Config, nil)
		}
		return p, nil

	case tea.KeyPressMsg:
		return p, p.onKey(m)
	}
	return p, nil
}

// onTick re-arms the base tick and issues a load when the cadence has elapsed (or a
// focus-triggered load is pending) and none is in flight. A refresh is dropped while a
// load/write is in flight (single-in-flight guard) so a slow direct-write transaction
// plus the periodic refresh cannot stack goroutines on the shared pool.
func (p *otaPane) onTick() tea.Cmd {
	if p.store == nil {
		return nil
	}
	cmds := []tea.Cmd{tickCmd()}
	due := p.forceLoad || p.lastLoadAt.IsZero() || time.Since(p.lastLoadAt) >= p.currentInterval()
	if due && !p.inFlight {
		cmds = append(cmds, p.issueLoad())
	}
	return tea.Batch(cmds...)
}

// onKey routes to the form when one is open, else to the table/action handler.
func (p *otaPane) onKey(k tea.KeyPressMsg) tea.Cmd {
	if p.form != nil {
		return p.onFormKey(k)
	}
	return p.onTableKey(k)
}

// onFormKey constrains the form to text entry + esc/enter + up/down (the caveat). esc
// cancels; enter submits; up/down move between fields; everything else goes to the
// focused input.
func (p *otaPane) onFormKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		p.form = nil
		return nil
	case "enter":
		return p.formSubmit()
	case "up":
		p.formFocusDelta(-1)
		return nil
	case "down":
		p.formFocusDelta(1)
		return nil
	default:
		if p.form.focus < 0 || p.form.focus >= len(p.form.fields) {
			return nil
		}
		var cmd tea.Cmd
		p.form.fields[p.form.focus].input, cmd = p.form.fields[p.form.focus].input.Update(k)
		return cmd
	}
}

// onTableKey handles navigation + the pane-local actions (only when focused and no form
// is open; the shell consumes global chrome keys first).
func (p *otaPane) onTableKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "up":
		p.moveCursor(-1)
		return nil
	case "down":
		p.moveCursor(1)
		return nil
	case "left", "right":
		p.toggleTable()
		return nil
	}

	switch k.String() {
	case p.keys.switchTable:
		p.toggleTable()
	case p.keys.refresh:
		if p.store != nil && !p.inFlight {
			return p.issueLoad()
		}
	case p.keys.register:
		return p.openRegister()
	case p.keys.setChannel:
		return p.openChannel()
	case p.keys.pin:
		return p.openPin()
	case p.keys.unpin:
		return p.unpinSelected()
	case p.keys.toggleState:
		return p.toggleSelectedState()
	case p.keys.markDone:
		return p.markSelectedDone()
	}
	return nil
}

// --- form open / scan / submit ---------------------------------------------------

func (p *otaPane) openRegister() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	p.form = &formState{
		kind:     formRegister,
		title:    "Register firmware",
		scanning: true,
		fields: []formField{
			{label: "version", input: newInput("scanning…", "")},
			{label: "notes", input: newInput("optional notes", "")},
		},
	}
	p.focusFormField()
	return p.scanArtifactCmd()
}

func (p *otaPane) openChannel() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	p.form = &formState{
		kind:  formChannel,
		title: "Set channel default",
		fields: []formField{
			{label: "channel", input: newInput("channel", p.cfg.OTA.DefaultChannel)},
			{label: "version", input: newInput("version", "")},
		},
	}
	p.focusFormField()
	return nil
}

func (p *otaPane) openPin() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	serial, channel := "", p.cfg.OTA.DefaultChannel
	if d, ok := p.selectedDevice(); ok {
		serial = d.Serial
		if d.Channel != "" {
			channel = d.Channel
		}
	}
	p.form = &formState{
		kind:  formPin,
		title: "Pin device to version",
		info:  "serial '*' targets the whole channel fleet",
		fields: []formField{
			{label: "serial", input: newInput("serial or *", serial)},
			{label: "channel", input: newInput("channel", channel)},
			{label: "version", input: newInput("version", "")},
		},
	}
	p.focusFormField()
	return nil
}

// writeReady reports whether a write backend exists, setting a status line otherwise so
// the operator learns why the form did not open (read-only mode).
func (p *otaPane) writeReady() bool {
	if p.writer == nil {
		p.status = "writes disabled — set ota.admin_api_url, or ota.allow_direct_write=true (needs read_only=false)"
		return false
	}
	return true
}

func (p *otaPane) onArtifactScanned(m fwArtifactScannedMsg) tea.Cmd {
	if p.form == nil || p.form.kind != formRegister {
		return nil
	}
	p.form.scanning = false
	if m.err != nil {
		p.form.errMsg = m.err.Error()
		return nil
	}
	a := m.art
	p.form.art = &a
	p.setFieldValue("version", a.Version)
	p.form.info = fmt.Sprintf("sha256 %s · %d bytes", a.SHA256, a.SizeBytes)
	return nil
}

func (p *otaPane) formSubmit() tea.Cmd {
	f := p.form
	switch f.kind {
	case formRegister:
		if f.art == nil {
			f.errMsg = "no artifact scanned (build the firmware first)"
			return nil
		}
		version := strings.TrimSpace(f.value("version"))
		if version == "" {
			f.errMsg = "version is required"
			return nil
		}
		notes := strings.TrimSpace(f.value("notes"))
		art := *f.art
		p.form = nil
		p.inFlight = true
		return p.registerFirmwareCmd(art, version, notes)

	case formChannel:
		ch := strings.TrimSpace(f.value("channel"))
		ver := strings.TrimSpace(f.value("version"))
		if ch == "" || ver == "" {
			f.errMsg = "channel and version are required"
			return nil
		}
		p.form = nil
		p.inFlight = true
		return p.setChannelCmd(SetChannelDefault{Channel: ch, Version: ver})

	case formPin:
		serial := strings.TrimSpace(f.value("serial"))
		ch := strings.TrimSpace(f.value("channel"))
		ver := strings.TrimSpace(f.value("version"))
		if serial == "" || ch == "" || ver == "" {
			f.errMsg = "serial, channel and version are required"
			return nil
		}
		p.form = nil
		p.inFlight = true
		return p.pinCmd(PinRollout{Serial: serial, Channel: ch, Version: ver, Pinned: true})
	}
	return nil
}

// --- rollout actions on the selected device --------------------------------------

func (p *otaPane) unpinSelected() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	d, ok := p.selectedDevice()
	if !ok || d.Rollout == nil || d.Rollout.Serial == "*" {
		p.status = "no per-serial pin on the selected device"
		return nil
	}
	p.inFlight = true
	// Re-submit the existing version with pinned=false (keeps the target active but
	// drops its pin priority — the literal meaning of "unpin").
	return p.pinCmd(PinRollout{Serial: d.Rollout.Serial, Channel: d.Rollout.Channel, Version: d.Rollout.Version, Pinned: false})
}

func (p *otaPane) toggleSelectedState() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	d, ok := p.selectedDevice()
	if !ok || d.Rollout == nil {
		p.status = "no rollout to pause/resume on the selected device"
		return nil
	}
	next := StatePaused
	if d.Rollout.State == StatePaused {
		next = StateActive
	}
	p.inFlight = true
	return p.setStateCmd(SetRolloutState{ID: d.Rollout.ID, State: next})
}

func (p *otaPane) markSelectedDone() tea.Cmd {
	if !p.writeReady() {
		return nil
	}
	d, ok := p.selectedDevice()
	if !ok || d.Rollout == nil {
		p.status = "no rollout to mark done on the selected device"
		return nil
	}
	p.inFlight = true
	return p.setStateCmd(SetRolloutState{ID: d.Rollout.ID, State: StateDone})
}

func (p *otaPane) onWriteResult(m writeResultMsg) tea.Cmd {
	p.inFlight = false
	if m.err != nil {
		p.status = m.kind + " failed: " + m.err.Error()
		p.markDirty()
		return nil
	}
	p.status = m.kind + " ok"
	if !p.Focused() {
		p.pendingWrite = true
	}
	p.markDirty()
	if p.store != nil {
		return p.issueLoad() // reflect the write
	}
	return nil
}

// --- cursor / selection helpers --------------------------------------------------

func (p *otaPane) toggleTable() {
	if p.active == tableVersions {
		p.active = tableDevices
	} else {
		p.active = tableVersions
	}
}

func (p *otaPane) moveCursor(delta int) {
	if p.active == tableVersions {
		p.verCur = clamp(p.verCur+delta, 0, max0(len(p.state.Versions)-1))
	} else {
		p.devCur = clamp(p.devCur+delta, 0, max0(len(p.state.Devices)-1))
	}
}

func (p *otaPane) clampCursors() {
	p.verCur = clamp(p.verCur, 0, max0(len(p.state.Versions)-1))
	p.devCur = clamp(p.devCur, 0, max0(len(p.state.Devices)-1))
}

func (p *otaPane) selectedDevice() (DeviceOTA, bool) {
	if p.devCur < 0 || p.devCur >= len(p.state.Devices) {
		return DeviceOTA{}, false
	}
	return p.state.Devices[p.devCur], true
}

func (p *otaPane) selectDeviceBySerial(serial string) {
	for i, d := range p.state.Devices {
		if d.Serial == serial {
			p.active = tableDevices
			p.devCur = i
			return
		}
	}
}

// --- form field focus helpers ----------------------------------------------------

func (p *otaPane) focusFormField() {
	if p.form == nil || !p.Focused() {
		return
	}
	for i := range p.form.fields {
		p.form.fields[i].input.Blur()
	}
	if p.form.focus >= 0 && p.form.focus < len(p.form.fields) {
		p.form.fields[p.form.focus].input.Focus()
	}
}

func (p *otaPane) blurFormField() {
	if p.form == nil {
		return
	}
	for i := range p.form.fields {
		p.form.fields[i].input.Blur()
	}
}

func (p *otaPane) formFocusDelta(d int) {
	if p.form == nil || len(p.form.fields) == 0 {
		return
	}
	p.form.fields[p.form.focus].input.Blur()
	p.form.focus = (p.form.focus + d + len(p.form.fields)) % len(p.form.fields)
	p.form.fields[p.form.focus].input.Focus()
}

func (p *otaPane) setFieldValue(label, val string) {
	if p.form == nil {
		return
	}
	for i := range p.form.fields {
		if p.form.fields[i].label == label {
			p.form.fields[i].input.SetValue(val)
			return
		}
	}
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

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
