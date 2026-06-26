package app

import (
	"context"
	"fmt"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// PaneFactory constructs a pane of one kind. The signature is fixed by K4: the
// root injects a pre-filled BasePane (fresh PaneID, per-pane ctx, shared Sender)
// and the current *config.Config; the factory returns a ready-to-Init pane.
//
// Spawn Args (host/port/DSN-bearing values from SpawnPaneMsg) are NOT part of this
// signature, so internal/app carries no per-kind arg schema and stays transport-
// free. Instead, right after a spawn the root delivers the args to the pane as an
// addressed PaneMsg{To:id, Payload: SpawnArgsMsg{Args}} (see update.go); a pane
// that needs args handles SpawnArgsMsg in its Update, a pane that does not ignores
// it. This keeps the factory uniform across all kinds.
type PaneFactory func(base pane.BasePane, cfg *config.Config) pane.Pane

// paneRegistry is the package-local registry (wm-shell §4.2 package note): a
// factory map keyed by kind, plus the ordered set of live panes keyed by id. It
// is mutated only on the message loop, so it needs no locking. The registry holds
// ALL panes including hidden/background ones; visibility is the layout's call.
type paneRegistry struct {
	factories map[pane.PaneKind]PaneFactory

	panes  map[pane.PaneID]pane.Pane          // id → live pane
	cancel map[pane.PaneID]context.CancelFunc // id → its per-pane ctx cancel
	order  []pane.PaneID                      // spawn order (drives reverse teardown)

	rootCtx context.Context
	send    pane.Sender
	cfg     *config.Config

	// seq counts spawns to mint unique PaneIDs (id values are runtime-derived,
	// never code literals — air-gap).
	seq int
}

func newRegistry(rootCtx context.Context, send pane.Sender, cfg *config.Config) *paneRegistry {
	return &paneRegistry{
		factories: map[pane.PaneKind]PaneFactory{},
		panes:     map[pane.PaneID]pane.Pane{},
		cancel:    map[pane.PaneID]context.CancelFunc{},
		rootCtx:   rootCtx,
		send:      send,
		cfg:       cfg,
	}
}

// register binds a factory to a kind. main.go calls this for every kind; a later
// duplicate registration overwrites (last wins), which a test can assert.
func (r *paneRegistry) register(kind pane.PaneKind, f PaneFactory) {
	r.factories[kind] = f
}

// setConfig swaps the *config.Config used for future spawns (K4 hot-reload).
func (r *paneRegistry) setConfig(cfg *config.Config) { r.cfg = cfg }

// mintID returns a fresh, unique PaneID for a kind. The value is derived from the
// kind plus a monotonic counter — runtime data, never a hardcoded literal.
func (r *paneRegistry) mintID(kind pane.PaneKind) pane.PaneID {
	r.seq++
	return pane.PaneID(fmt.Sprintf("%s:%d", kind, r.seq))
}

// spawn constructs and registers a pane of kind. It mints an id, derives a
// per-pane context from rootCtx, builds the BasePane, invokes the factory and
// records the pane in spawn order. It returns the new pane and its id, or an
// error if no factory is registered for the kind.
func (r *paneRegistry) spawn(kind pane.PaneKind) (pane.Pane, pane.PaneID, error) {
	f, ok := r.factories[kind]
	if !ok {
		return nil, "", fmt.Errorf("no factory registered for pane kind %q", kind)
	}
	id := r.mintID(kind)
	ctx, cancel := context.WithCancel(r.rootCtx)
	base := pane.NewBase(id, kind, ctx, r.send)
	p := f(base, r.cfg)
	if p == nil {
		cancel()
		return nil, "", fmt.Errorf("factory for kind %q returned nil", kind)
	}
	r.panes[id] = p
	r.cancel[id] = cancel
	r.order = append(r.order, id)
	return p, id, nil
}

// lookup returns the live pane for an id (nil,false if absent/closed).
func (r *paneRegistry) lookup(id pane.PaneID) (pane.Pane, bool) {
	p, ok := r.panes[id]
	return p, ok
}

// all returns the live id→pane map (read-only; for layout composition).
func (r *paneRegistry) all() map[pane.PaneID]pane.Pane { return r.panes }

// orderIDs returns the live panes in spawn order (read-only).
func (r *paneRegistry) orderIDs() []pane.PaneID { return r.order }

// remove cancels a pane's context, calls Close() once, and drops it from the
// registry. It returns the Close error (nil if absent). The ctx is cancelled
// BEFORE removal so an in-flight goroutine's Send lands on a now-unknown id and is
// dropped by the router (R3), not delivered to a half-closed pane.
func (r *paneRegistry) remove(id pane.PaneID) error {
	p, ok := r.panes[id]
	if !ok {
		return nil
	}
	if cancel, ok := r.cancel[id]; ok {
		cancel()
	}
	err := p.Close()
	delete(r.panes, id)
	delete(r.cancel, id)
	for i, oid := range r.order {
		if oid == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return err
}

// len returns the number of live panes.
func (r *paneRegistry) len() int { return len(r.panes) }
