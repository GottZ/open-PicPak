package logs

import "sort"

// filterOverlay is the device-multiselect state (the device_filter action). It is a
// view-loop value (mutated only in Update); applying it hands a new Filter to the store,
// which reloads in its goroutine. An empty selection means "all devices".
type filterOverlay struct {
	open     bool
	serials  []string        // candidate serials (sorted, de-duplicated)
	selected map[string]bool // which are checked
	cursor   int
}

// openWith populates the overlay from the candidate serials and seeds the checkmarks from
// the currently active serial filter (empty current = nothing checked = "all").
func (f *filterOverlay) openWith(candidates, current []string) {
	f.serials = dedupeSorted(candidates)
	f.selected = map[string]bool{}
	for _, s := range current {
		f.selected[s] = true
	}
	f.cursor = 0
	f.open = true
}

// move advances the overlay cursor, clamped to the candidate list.
func (f *filterOverlay) move(delta int) {
	if len(f.serials) == 0 {
		f.cursor = 0
		return
	}
	f.cursor += delta
	if f.cursor < 0 {
		f.cursor = 0
	}
	if f.cursor >= len(f.serials) {
		f.cursor = len(f.serials) - 1
	}
}

// toggle flips the checkmark under the cursor.
func (f *filterOverlay) toggle() {
	if f.cursor < 0 || f.cursor >= len(f.serials) {
		return
	}
	s := f.serials[f.cursor]
	f.selected[s] = !f.selected[s]
}

// chosen returns the checked serials as the new filter list (sorted; empty = all).
func (f *filterOverlay) chosen() []string {
	var out []string
	for s, on := range f.selected {
		if on {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// close hides the overlay without applying.
func (f *filterOverlay) close() { f.open = false }

// cycleSingle implements the all → item[0] → item[1] → … → all rotation used by both the
// source cycle and the jump-to-serial single-isolate. An empty `current` is the "all"
// state (state 0); a single matching item is state i+1; the next state wraps back to all
// after the last item. Returns nil for the "all" state.
func cycleSingle(items, current []string) []string {
	if len(items) == 0 {
		return nil
	}
	idx := 0 // 0 == all
	if len(current) == 1 {
		for i, s := range items {
			if s == current[0] {
				idx = i + 1
				break
			}
		}
	}
	next := idx + 1
	if next > len(items) {
		next = 0
	}
	if next == 0 {
		return nil // all
	}
	return []string{items[next-1]}
}

// dedupeSorted returns the unique, sorted set of a string slice (drops empties).
func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
