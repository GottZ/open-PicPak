package app

import (
	"time"

	"github.com/open-picpak/picpak-ops/internal/pane"
)

// teardown is the single graceful-shutdown path (wm-shell §2.4, §4 teardown.go):
// it cancels every per-pane context and calls Pane.Close() exactly once, in
// REVERSE spawn order (later panes — which may depend on earlier ones — close
// first), bounded by ui.teardown_deadline so a wedged Close() cannot hang quit.
//
// It runs OUTSIDE the message loop (Close returns error, not tea.Cmd — §4.1), so
// it blocks until all panes close or the deadline elapses, then returns; the
// caller (beginQuit) follows with tea.Quit. After the deadline, remaining panes
// are abandoned: their contexts are already cancelled, so their goroutines unwind
// on their own and the program exit + rootCtx cancel drops any remaining fds.
func (m *Model) teardown() {
	deadline := m.cfg.UI.TeardownDeadline.D()
	if deadline <= 0 {
		// No deadline configured → close synchronously without a timer.
		m.closeAllReverse()
		return
	}

	done := make(chan struct{})
	go func() {
		m.closeAllReverse()
		close(done)
	}()

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// Deadline hit: abandon the rest. rootCtx was already cancelled (cancel()
		// below is idempotent) so every pane goroutine is unwinding.
	}
	if m.cancel != nil {
		m.cancel()
	}
}

// closeAllReverse closes every live pane in reverse spawn order, each exactly
// once. registry.remove cancels the pane ctx before Close() (R3) and drops it from
// the registry, so a second pass would find nothing — Close is idempotent and
// called once per pane.
func (m *Model) closeAllReverse() {
	ids := m.reg.orderIDs()
	// Snapshot because remove mutates the order slice.
	snap := make([]pane.PaneID, len(ids))
	copy(snap, ids)
	for i := len(snap) - 1; i >= 0; i-- {
		_ = m.reg.remove(snap[i])
	}
}
