package layout

// Op is a layout operation requested via the shell taxonomy (LayoutMsg). Tabs-first
// today (next/prev); split ops are reserved for a later refinement (OQ#2) and are
// parsed-but-inert until then.
type Op int

const (
	OpNextTab Op = iota // focus the next tab (wrap)
	OpPrevTab           // focus the previous tab (wrap)
	OpSplit             // reserved: split the current tab (no-op until splits land)
)
