package pane

import "sync"

// Scrollback is a bounded, goroutine-safe ring buffer of rendered lines, shared
// by the stream panes (log viewer, device console, build output) so two axes do
// not reinvent scrollback. It is safe to Append from a background goroutine
// while View reads via Lines on the message loop; capacity bounds memory so a
// long-backgrounded pane cannot grow unbounded.
type Scrollback struct {
	mu    sync.Mutex
	buf   []string
	cap   int
	start int // index of the oldest line
	count int // number of valid lines (<= cap)
}

// NewScrollback returns a ring holding at most capacity lines (minimum 1).
func NewScrollback(capacity int) *Scrollback {
	if capacity < 1 {
		capacity = 1
	}
	return &Scrollback{buf: make([]string, capacity), cap: capacity}
}

// Append adds one line, evicting the oldest when full.
func (s *Scrollback) Append(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendLocked(line)
}

// AppendBatch adds many lines in one lock acquisition (the coalesced-output path).
func (s *Scrollback) AppendBatch(lines []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range lines {
		s.appendLocked(l)
	}
}

func (s *Scrollback) appendLocked(line string) {
	if s.count < s.cap {
		s.buf[(s.start+s.count)%s.cap] = line
		s.count++
		return
	}
	// full: overwrite oldest and advance start
	s.buf[s.start] = line
	s.start = (s.start + 1) % s.cap
}

// Lines returns a snapshot of the buffered lines in chronological order.
func (s *Scrollback) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, s.count)
	for i := 0; i < s.count; i++ {
		out[i] = s.buf[(s.start+i)%s.cap]
	}
	return out
}

// Len returns the number of buffered lines.
func (s *Scrollback) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Cap returns the ring capacity.
func (s *Scrollback) Cap() int { return s.cap }

// Clear drops all buffered lines (view-only reset; never touches a data source).
func (s *Scrollback) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.start, s.count = 0, 0
}
