package flash

import "github.com/open-picpak/picpak-ops/internal/sshhost"

// These are the tea.Msg payloads the controller goroutines emit to the flash pane as
// an addressed app.PaneMsg{To: flashPaneID} via the injected Sender (K2). They never
// touch Model state directly; the pane folds them into its per-device matrix whether
// it is focused or backgrounded. The controller emits ONLY these addressed payloads —
// plus the ONE top-level app.ReleaseDevicePortMsg (K6), which is not pane-addressed.

// FlashStartedMsg announces a run was accepted and the matrix should initialize one
// row per target.
type FlashStartedMsg struct {
	Targets []Target
}

// FlashPhaseMsg moves one device's row to a new lifecycle phase (validating →
// staging → confirming → flashing).
type FlashPhaseMsg struct {
	TargetID TargetID
	Phase    Phase
}

// FlashRunStartedMsg ties a device to the sshhost RunID now streaming its esptool
// output, so the pane can attribute incoming RunOutputMsg lines to the right row for
// live progress. NumFiles is the verified-all denominator.
type FlashRunStartedMsg struct {
	TargetID TargetID
	RunID    sshhost.RunID
	NumFiles int
}

// FlashDeviceDoneMsg is one device's TERMINAL result — the authoritative verdict the
// controller computes after the run drained (every file verified AND exit 0 → OK).
// A failure carries its class + hint + the bounded output tail.
type FlashDeviceDoneMsg struct {
	TargetID TargetID
	OK       bool
	Class    FailClass
	Hint     string
	Tail     []string
}

// FlashBatchDoneMsg fires once every target reached a terminal phase. Summary is the
// per-device verdict map — NEVER a single batch bool; the matrix rows remain the
// source of truth.
type FlashBatchDoneMsg struct {
	Summary map[TargetID]bool
}
