package pane

import "context"

// BasePane is an embeddable helper that stores the common pane fields (id, kind,
// size, focus flag, per-pane context, Sender) and supplies default SetSize,
// SetFocused and Meta. An axis embeds it (by value) and implements only
// Init/Update/View/Close — and overrides Meta when it needs a live
// title/status/dirty.
//
// The embedding pane must be used as a pointer (*concretePane) so the
// pointer-receiver mutators below promote and the type satisfies Pane.
type BasePane struct {
	id      PaneID
	kind    PaneKind
	width   int
	height  int
	focused bool
	ctx     context.Context
	send    Sender
}

// NewBase builds the common pane state the root injects at spawn: a fresh
// PaneID, the pane's kind, a per-pane context (child of the root context,
// cancelled on teardown) and the shared Sender bridge.
func NewBase(id PaneID, kind PaneKind, ctx context.Context, send Sender) BasePane {
	return BasePane{id: id, kind: kind, ctx: ctx, send: send}
}

// ID returns the pane's stable identifier.
func (b *BasePane) ID() PaneID { return b.id }

// Kind returns the pane's feature kind.
func (b *BasePane) Kind() PaneKind { return b.kind }

// Context returns the per-pane context; a pane binds its goroutines/transports
// to it so they cancel on Close()/teardown.
func (b *BasePane) Context() context.Context { return b.ctx }

// Sender returns the goroutine-safe bridge to the program message loop.
func (b *BasePane) Sender() Sender { return b.send }

// Size returns the last size set via SetSize.
func (b *BasePane) Size() (width, height int) { return b.width, b.height }

// Focused reports whether the pane currently owns input.
func (b *BasePane) Focused() bool { return b.focused }

// SetSize records the latest size; panes precompute layout here instead of in View.
func (b *BasePane) SetSize(width, height int) { b.width, b.height = width, height }

// SetFocused records focus. Cosmetic/state only — never opens or closes a transport.
func (b *BasePane) SetFocused(focused bool) { b.focused = focused }

// Meta returns a default chrome view (Idle, no title). Panes with live state
// override this to supply Title/Status/Dirty.
func (b *BasePane) Meta() PaneMeta {
	return PaneMeta{ID: b.id, Kind: b.kind, Status: StatusIdle}
}
