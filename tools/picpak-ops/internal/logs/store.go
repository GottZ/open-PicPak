package logs

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/app"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// logStore is the long-lived tail engine behind the logs pane. It owns ONE goroutine
// that drives its own cadence (poll_interval, K3): it primes the ring with the newest
// page, then forward-polls a monotone keyset cursor, parses each row's payload into
// lines, and keeps a bounded in-store ring. Every result is emitted as an addressed
// app.PaneMsg through the injected Sender (K2), so the store keeps tailing while the
// pane is backgrounded. The goroutine lives for the pane's whole life and stops only on
// ctx cancel (Close), never on blur.
//
// Concurrency: the goroutine is the only writer of ring/cursor; the message loop reads
// the ring via Snapshot. A single mutex guards the shared fields. Because polls and
// backfills run synchronously inside the one goroutine loop, there is at most one query
// in flight (the design's single-flight, by construction — a tick that fires during a
// long query is coalesced by the timer).
type logStore struct {
	ctx  context.Context
	pool *pgxpool.Pool // nil in unit tests that inject rows directly; guarded everywhere
	id   pane.PaneID
	send pane.Sender
	cfg  Config

	reqCh   chan storeReq
	started bool

	mu                sync.Mutex
	ring              []Line
	cursor            Cursor
	seenAtMax         map[string]struct{} // de-dup keys for rows AT exactly cursor.Time
	oldest            *time.Time          // backfill low-water (nil until primed)
	fwdBoot           *bootTracker        // per-serial last boot_count for the forward tail
	filter            Filter
	primed            bool
	backfillExhausted bool
	backfilledRows    int

	curBackoff time.Duration
}

// reqKind enumerates the model→store requests (run synchronously in the goroutine).
type reqKind int

const (
	reqBackfill reqKind = iota
	reqSetFilter
	reqReload
	reqClear
)

type storeReq struct {
	kind   reqKind
	filter Filter
}

// newLogStore builds a store bound to the per-pane ctx, the shared read pool, and the
// pane's id+Sender (so its messages route back addressed). It does NOT start the
// goroutine — the pane's Init does, once.
func newLogStore(ctx context.Context, pool *pgxpool.Pool, id pane.PaneID, send pane.Sender, cfg Config) *logStore {
	return &logStore{
		ctx:       ctx,
		pool:      pool,
		id:        id,
		send:      send,
		cfg:       cfg,
		reqCh:     make(chan storeReq, 16),
		seenAtMax: map[string]struct{}{},
		fwdBoot:   newBootTracker(),
		filter: Filter{
			Serials: append([]string(nil), cfg.DefaultSerials...),
			Sources: append([]string(nil), cfg.DefaultSources...),
		},
	}
}

// Start launches the tail goroutine exactly once.
func (s *logStore) Start() {
	if s.started || s.pool == nil {
		return
	}
	s.started = true
	go s.run()
}

// run is the single cadence loop: a self-rescheduling timer drives prime→forward polls;
// the request channel handles backfill/filter/reload/clear; ctx cancel ends it.
func (s *logStore) run() {
	timer := time.NewTimer(0) // prime immediately
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case r := <-s.reqCh:
			s.handleReq(r)
		case <-timer.C:
			timer.Reset(s.tick())
		}
	}
}

// tick runs one poll (prime if not yet primed, else forward) and returns the wait until
// the next poll: poll_interval on success, an exponential backoff (capped) on error.
func (s *logStore) tick() time.Duration {
	if s.pool == nil {
		return s.pollInterval()
	}
	var err error
	if !s.isPrimed() {
		err = s.prime()
	} else {
		err = s.forward()
	}
	if err != nil {
		s.emit(logErrMsg{err: err})
		s.curBackoff = nextBackoff(s.curBackoff, s.cfg.ErrorBackoff, s.cfg.ErrorBackoffMax)
		return s.curBackoff
	}
	s.curBackoff = 0
	return s.pollInterval()
}

// prime loads the newest page for the active filter, sets the cursor to its newest row,
// records the oldest loaded time (the backfill low-water), and refreshes the serial
// picker. It is the cold-start AND the post-filter/reload reload path.
func (s *logStore) prime() error {
	f := s.currentFilter()
	ctx, cancel := s.queryCtx()
	defer cancel()

	rows, err := queryRecent(ctx, s.pool, f, s.cfg.BackfillPage)
	if err != nil {
		return err
	}
	added := s.ingestForward(rows)

	s.mu.Lock()
	if len(rows) > 0 {
		t := rows[0].Time // chronological → oldest in the page
		s.oldest = &t
	}
	s.primed = true
	s.backfillExhausted = len(rows) < s.cfg.BackfillPage
	cur := s.cursor
	s.mu.Unlock()

	if serials, serr := queryDistinctSerials(ctx, s.pool); serr == nil {
		s.emit(logSerialsMsg{serials: serials})
	}
	s.emit(logRowsMsg{added: added, cur: cur})
	return nil
}

// forward polls everything at-or-after the cursor time and ingests the genuinely new
// rows (the de-dup set drops the high-water-second rows already held).
func (s *logStore) forward() error {
	s.mu.Lock()
	since := s.cursor.Time
	s.mu.Unlock()
	f := s.currentFilter()

	ctx, cancel := s.queryCtx()
	defer cancel()
	rows, err := queryForward(ctx, s.pool, f, since, s.cfg.PollBatch)
	if err != nil {
		return err
	}
	added := s.ingestForward(rows)
	if added > 0 {
		s.emit(logRowsMsg{added: added, cur: s.cursorCopy()})
	}
	return nil
}

// doBackfill prepends one older page when there is more history to load and the history
// cap is not yet reached; it always emits a logBackfillMsg (done=true when exhausted).
func (s *logStore) doBackfill() error {
	s.mu.Lock()
	if s.pool == nil || s.oldest == nil || s.backfillExhausted || s.backfilledRows >= s.cfg.HistoryMaxRows {
		s.mu.Unlock()
		s.emit(logBackfillMsg{added: 0, done: true})
		return nil
	}
	before := *s.oldest
	s.mu.Unlock()
	f := s.currentFilter()

	ctx, cancel := s.queryCtx()
	defer cancel()
	rows, err := queryBackfill(ctx, s.pool, f, before, s.cfg.BackfillPage)
	if err != nil {
		return err
	}
	added := s.ingestBackfill(rows)

	s.mu.Lock()
	if len(rows) > 0 {
		t := rows[0].Time
		s.oldest = &t
	}
	s.backfilledRows += len(rows)
	done := len(rows) < s.cfg.BackfillPage || s.backfilledRows >= s.cfg.HistoryMaxRows
	s.backfillExhausted = done
	s.mu.Unlock()

	s.emit(logBackfillMsg{added: added, done: done})
	return nil
}

// handleReq applies a model request in the goroutine (serialized with polls).
func (s *logStore) handleReq(r storeReq) {
	switch r.kind {
	case reqBackfill:
		if err := s.doBackfill(); err != nil {
			s.emit(logErrMsg{err: err})
		}
	case reqSetFilter:
		s.resetForReprime(&r.filter)
		if err := s.prime(); err != nil {
			s.emit(logErrMsg{err: err})
		}
	case reqReload:
		s.resetForReprime(nil)
		if err := s.prime(); err != nil {
			s.emit(logErrMsg{err: err})
		}
	case reqClear:
		// View-only clear: drop the ring but KEEP the cursor so the forward tail
		// continues from where it was. Never deletes DB rows.
		s.mu.Lock()
		s.ring = nil
		cur := s.cursor
		s.mu.Unlock()
		s.emit(logRowsMsg{added: 0, cur: cur})
	}
}

// resetForReprime clears the ring/cursor/de-dup/boot state (optionally swapping the
// filter) so the next prime starts fresh.
func (s *logStore) resetForReprime(f *Filter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f != nil {
		s.filter = Filter{
			Serials: append([]string(nil), f.Serials...),
			Sources: append([]string(nil), f.Sources...),
		}
	}
	s.ring = nil
	s.cursor = Cursor{}
	s.seenAtMax = map[string]struct{}{}
	s.oldest = nil
	s.fwdBoot = newBootTracker()
	s.primed = false
	s.backfillExhausted = false
	s.backfilledRows = 0
}

// ingestForward parses ascending rows into the ring, advancing the high-water cursor and
// de-duping exact repeats at the high-water second (the NULL-seq degradation, design Q3).
// It is the testable core: tests call it directly with a nil pool to drive the ring.
func (s *logStore) ingestForward(rows []logRow) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for _, r := range rows {
		// De-dup: a forward poll re-reads the high-water second; drop rows already held.
		if s.cursor.HasData && !r.Time.After(s.cursor.Time) {
			if _, dup := s.seenAtMax[dedupKey(r)]; dup {
				continue
			}
		}
		// Advance the high-water mark; reset the de-dup set when the second moves on.
		if !s.cursor.HasData || r.Time.After(s.cursor.Time) {
			s.cursor.Time = r.Time
			s.cursor.HasData = true
			s.seenAtMax = map[string]struct{}{}
		}
		if r.Time.Equal(s.cursor.Time) {
			s.seenAtMax[dedupKey(r)] = struct{}{}
		}
		if r.Seq != nil {
			s.cursor.Seq = r.Seq
		}
		lines := parseRow(s.cfg, r, s.fwdBoot)
		s.appendLinesLocked(lines)
		added += len(lines)
	}
	return added
}

// ingestBackfill parses an older page (with a fresh per-batch boot tracker) and prepends
// it to the ring.
func (s *logStore) ingestBackfill(rows []logRow) int {
	if len(rows) == 0 {
		return 0
	}
	bt := newBootTracker()
	var block []Line
	for _, r := range rows {
		block = append(block, parseRow(s.cfg, r, bt)...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring = append(block, s.ring...)
	s.trimLocked()
	return len(block)
}

// appendLinesLocked appends forward lines and FIFO-evicts the oldest beyond the live ring
// cap (the memory bound). mu must be held.
func (s *logStore) appendLinesLocked(lines []Line) {
	if len(lines) == 0 {
		return
	}
	s.ring = append(s.ring, lines...)
	s.trimLocked()
}

// trimLocked enforces the live_ring_lines cap by dropping the oldest lines. mu held.
func (s *logStore) trimLocked() {
	limit := s.cfg.LiveRingLines
	if limit < 1 {
		limit = 1
	}
	if len(s.ring) > limit {
		over := len(s.ring) - limit
		s.ring = append([]Line(nil), s.ring[over:]...)
	}
}

// Snapshot returns a copy of the current ring in chronological order. Safe to call from
// the message loop while the goroutine appends.
func (s *logStore) Snapshot() []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Line, len(s.ring))
	copy(out, s.ring)
	return out
}

// Len reports the number of lines currently in the ring.
func (s *logStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ring)
}

// BackfillDone reports whether the retention edge / history cap was reached (the model
// stops requesting backfill once true).
func (s *logStore) BackfillDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backfillExhausted
}

// FilterSerials returns the currently filtered serials (empty = all).
func (s *logStore) FilterSerials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.filter.Serials...)
}

// FilterSources returns the currently filtered sources (empty = all).
func (s *logStore) FilterSources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.filter.Sources...)
}

// SetFilter swaps the row filter and triggers a fresh prime (non-blocking; the heavy
// reload happens in the goroutine).
func (s *logStore) SetFilter(f Filter) { s.request(storeReq{kind: reqSetFilter, filter: f}) }

// RequestBackfill asks for one older page (non-blocking; ignored if the goroutine is busy
// — the model re-requests on the next scroll-to-top).
func (s *logStore) RequestBackfill() { s.request(storeReq{kind: reqBackfill}) }

// Reload resets the cursor/ring and re-primes (the reload key).
func (s *logStore) Reload() { s.request(storeReq{kind: reqReload}) }

// Clear drops the view ring (view-only; keeps the cursor so the tail continues).
func (s *logStore) Clear() { s.request(storeReq{kind: reqClear}) }

// request enqueues a model request; a full channel (goroutine busy) drops it, which is
// safe — filter/reload are re-pressable and backfill recurs on the next scroll.
func (s *logStore) request(r storeReq) {
	select {
	case s.reqCh <- r:
	default:
	}
}

// currentFilter returns a deep copy of the active filter (acquires mu; never call while
// holding mu).
func (s *logStore) currentFilter() Filter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Filter{
		Serials: append([]string(nil), s.filter.Serials...),
		Sources: append([]string(nil), s.filter.Sources...),
	}
}

func (s *logStore) isPrimed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primed
}

func (s *logStore) cursorCopy() Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func (s *logStore) pollInterval() time.Duration {
	if s.cfg.PollInterval > 0 {
		return s.cfg.PollInterval
	}
	return 3 * time.Second
}

// queryCtx derives a per-query context bounded by query_timeout from the per-pane ctx, so
// Close()/quit cancels an in-flight SELECT.
func (s *logStore) queryCtx() (context.Context, context.CancelFunc) {
	if s.cfg.QueryTimeout <= 0 {
		return context.WithCancel(s.ctx)
	}
	return context.WithTimeout(s.ctx, s.cfg.QueryTimeout)
}

// emit sends a store message back to this pane as an addressed PaneMsg (K2). A nil Sender
// (headless unit tests) is a no-op.
func (s *logStore) emit(payload tea.Msg) {
	if s.send == nil {
		return
	}
	s.send(app.PaneMsg{To: s.id, Payload: payload})
}

// nextBackoff doubles the current backoff toward the configured cap, seeding from the
// base on the first error.
func nextBackoff(cur, base, max time.Duration) time.Duration {
	if cur <= 0 {
		if base <= 0 {
			base = 10 * time.Second
		}
		return base
	}
	n := cur * 2
	if max > 0 && n > max {
		n = max
	}
	return n
}
