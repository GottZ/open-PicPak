package logs

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-picpak/picpak-ops/internal/config"
	"github.com/open-picpak/picpak-ops/internal/fleet"
	"github.com/open-picpak/picpak-ops/internal/pane"
)

// --- fixtures ---------------------------------------------------------------

// fullConfig is a *config.Config with the [logs] section populated with test-stable
// values (ASCII markers + no colorize, so substring asserts are not perturbed by ANSI).
// It exercises the real configFrom mapping the factory uses.
func fullConfig() *config.Config {
	return &config.Config{
		Theme: map[string]string{},
		Logs: config.Logs{
			LineSeparator:      "|",
			PollInterval:       config.Duration(50 * time.Millisecond),
			PollBatch:          500,
			BackfillPage:       200,
			LiveRingLines:      5000,
			HistoryMaxRows:     50000,
			DefaultSources:     []string{"telemetry", "ota-snapshot"},
			ShowGapMarkers:     true,
			GapMarkerFormat:    "-- boot {from} -> {to} (gap) --",
			ShowSuspectMarkers: true,
			SuspectMarker:      "SUSPECT",
			FollowDefault:      true,
			ErrorBackoff:       config.Duration(10 * time.Millisecond),
			ErrorBackoffMax:    config.Duration(100 * time.Millisecond),
			TimestampFormat:    "15:04:05",
			Timezone:           "Local",
			ColorizeBy:         "none",
			Palette:            nil,
			QueryTimeout:       config.Duration(5 * time.Second),
			Keys: map[string][]string{
				"follow": {"f"}, "device_filter": {"d"}, "source_cycle": {"s"},
				"clear_view": {"x"}, "copy": {"y"}, "jump_serial": {"g"}, "reload": {"R"},
			},
		},
	}
}

// newTestPane builds a logs pane through the real factory with a nil pool (so store is
// nil), sized like a real spawn.
func newTestPane(t *testing.T, cfg *config.Config) *logsPane {
	t.Helper()
	base := pane.NewBase(pane.PaneID("logs:test"), pane.KindLogs, context.Background(), func(tea.Msg) {})
	p, ok := New(nil, fleet.New(nil))(base, cfg).(*logsPane)
	if !ok {
		t.Fatal("factory did not return *logsPane")
	}
	p.SetSize(120, 24)
	return p
}

func bcPtr(v int64) *int64  { return &v }
func offPtr(v int64) *int64 { return &v }

func strsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- splitPayload (the single load-bearing parse) ---------------------------

func TestSplitPayload(t *testing.T) {
	cases := []struct {
		name, in, sep string
		want          []string
	}{
		{"three-lines", "boot42|wifi:connected|got ip", "|", []string{"boot42", "wifi:connected", "got ip"}},
		{"trailing-sep", "a|b|", "|", []string{"a", "b"}},
		{"empty", "", "|", nil},
		{"no-sep", "abc", "|", []string{"abc"}},
		{"custom-sep", "a;;b;;c;;", ";;", []string{"a", "b", "c"}},
		{"interior-empty-kept", "a||b", "|", []string{"a", "", "b"}},
		// Only ONE trailing empty token is dropped (design §2 step 3), so "|" leaves a
		// single empty content line — not an empty slice.
		{"only-sep-leaves-one-empty", "|", "|", []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitPayload(c.in, c.sep)
			if !strsEqual(got, c.want) {
				t.Fatalf("splitPayload(%q, %q) = %q, want %q", c.in, c.sep, got, c.want)
			}
		})
	}
}

// --- cursor monotonicity + de-dup with NULL seq -----------------------------

func TestIngestForward_DedupAndCursor(t *testing.T) {
	st := newLogStore(context.Background(), nil, "logs:test", nil, configFrom(fullConfig()))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)

	// Two distinct same-second rows (seq NULL; distinguished by offset_start + payload).
	batch := []logRow{
		{Time: t0, Serial: "AAAAAAAA", Payload: "l1", Source: "telemetry", OffsetStart: offPtr(0)},
		{Time: t0, Serial: "AAAAAAAA", Payload: "l2", Source: "telemetry", OffsetStart: offPtr(10)},
	}
	if added := st.ingestForward(batch); added != 2 {
		t.Fatalf("first ingest: added=%d, want 2", added)
	}
	if !st.cursor.HasData || !st.cursor.Time.Equal(t0) {
		t.Fatalf("cursor after first ingest = %v, want %v", st.cursor.Time, t0)
	}

	// A forward poll re-reads time>=cursor (the same second); both rows must be dropped.
	if added := st.ingestForward(batch); added != 0 {
		t.Fatalf("re-ingest of the high-water second must de-dup: added=%d, want 0", added)
	}
	if st.Len() != 2 {
		t.Fatalf("ring should still hold 2 lines after de-dup, got %d", st.Len())
	}

	// A genuinely new same-second row (new offset/payload) is kept.
	if added := st.ingestForward([]logRow{
		{Time: t0, Serial: "AAAAAAAA", Payload: "l3", Source: "telemetry", OffsetStart: offPtr(20)},
	}); added != 1 {
		t.Fatalf("distinct same-second row must be kept: added=%d, want 1", added)
	}

	// Advance to t1, then feed an out-of-order t0 row: the cursor must NOT go backward.
	st.ingestForward([]logRow{{Time: t1, Serial: "AAAAAAAA", Payload: "l4", Source: "telemetry", OffsetStart: offPtr(0)}})
	if !st.cursor.Time.Equal(t1) {
		t.Fatalf("cursor = %v after t1, want %v", st.cursor.Time, t1)
	}
	st.ingestForward([]logRow{{Time: t0, Serial: "AAAAAAAA", Payload: "late", Source: "telemetry", OffsetStart: offPtr(99)}})
	if !st.cursor.Time.Equal(t1) {
		t.Fatalf("cursor went backward to %v, must stay at %v (monotone)", st.cursor.Time, t1)
	}
}

// --- gap + suspect synthetic markers (from the config templates) ------------

func TestParseRow_GapAndSuspectMarkers(t *testing.T) {
	cfg := configFrom(fullConfig())
	bt := newBootTracker()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// First epoch: no gap divider before the first observed boot.
	l1 := parseRow(cfg, logRow{Time: t0, Serial: "AAAAAAAA", Payload: "line-a", BootCount: bcPtr(1), Source: "telemetry"}, bt)
	if len(l1) != 1 || l1[0].Synthetic != SynthNone {
		t.Fatalf("first row must be 1 content line, got %+v", l1)
	}

	// boot_count 1→2 injects a gap divider from the config template.
	l2 := parseRow(cfg, logRow{Time: t0.Add(time.Second), Serial: "AAAAAAAA", Payload: "line-b", BootCount: bcPtr(2), Source: "telemetry"}, bt)
	if len(l2) != 2 || l2[0].Synthetic != SynthGap {
		t.Fatalf("boot change must inject a gap divider first, got %+v", l2)
	}
	if want := "-- boot 1 -> 2 (gap) --"; l2[0].Text != want {
		t.Fatalf("gap marker text = %q, want %q (from gap_marker_format)", l2[0].Text, want)
	}

	// suspect=true injects the suspect marker (no boot change here).
	ls := parseRow(cfg, logRow{Time: t0.Add(2 * time.Second), Serial: "AAAAAAAA", Payload: "line-c", BootCount: bcPtr(2), Suspect: true, Source: "telemetry"}, bt)
	var sawSuspect bool
	for _, l := range ls {
		if l.Synthetic == SynthSuspect {
			sawSuspect = true
			if l.Text != cfg.SuspectMarker {
				t.Fatalf("suspect marker text = %q, want %q", l.Text, cfg.SuspectMarker)
			}
		}
	}
	if !sawSuspect {
		t.Fatalf("suspect=true must inject a suspect marker, got %+v", ls)
	}

	// Toggling the markers off suppresses injection (Policy=Data switch).
	cfg.ShowGapMarkers = false
	cfg.ShowSuspect = false
	bt2 := newBootTracker()
	parseRow(cfg, logRow{Time: t0, Serial: "AAAAAAAA", Payload: "x", BootCount: bcPtr(1), Source: "telemetry"}, bt2)
	off := parseRow(cfg, logRow{Time: t0, Serial: "AAAAAAAA", Payload: "y", BootCount: bcPtr(2), Suspect: true, Source: "telemetry"}, bt2)
	if len(off) != 1 || off[0].Synthetic != SynthNone {
		t.Fatalf("markers off must suppress injection, got %+v", off)
	}
}

// --- background-tail lifecycle (the load-bearing invariant) ------------------

// TestBackgroundTail proves the warm-ring contract: with the pane HIDDEN, rows ingested
// into the store (as its goroutine would) are present on refocus. A pane that paused on
// hide would render an empty body here.
func TestBackgroundTail(t *testing.T) {
	cfg := fullConfig()
	p := newTestPane(t, cfg)
	// Inject a real store (nil pool — we drive it directly, no DB) as Init would build.
	p.store = newLogStore(context.Background(), nil, p.ID(), func(tea.Msg) {}, configFrom(cfg))

	p.SetFocused(false) // pane is backgrounded
	// The store keeps tailing while hidden: feed it rows (the goroutine's append path).
	p.store.ingestForward([]logRow{
		{Time: time.Now(), Serial: "AAAAAAAA", Payload: "boot|wifi up|fetch ok", Source: "telemetry", BootCount: bcPtr(7)},
	})
	if p.store.Len() != 3 {
		t.Fatalf("store should hold 3 split lines while hidden, got %d", p.store.Len())
	}

	p.SetFocused(true) // returning to the pane shows the accumulated lines
	out := p.View()
	for _, want := range []string{"boot", "wifi up", "fetch ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("background-tail: refocus view missing %q in:\n%s", want, out)
		}
	}
}

// TestClearKeepsTailing proves clear_view is view-only: it empties the ring but keeps the
// cursor so the forward tail continues (it never deletes DB rows / never resets the tail).
func TestClearIsViewOnly(t *testing.T) {
	st := newLogStore(context.Background(), nil, "logs:test", nil, configFrom(fullConfig()))
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st.ingestForward([]logRow{{Time: t0, Serial: "AAAAAAAA", Payload: "a|b", Source: "telemetry", OffsetStart: offPtr(0)}})
	curBefore := st.cursorCopy()
	st.mu.Lock()
	st.ring = nil // the reqClear effect (view-only)
	st.mu.Unlock()
	if st.Len() != 0 {
		t.Fatalf("cleared ring should be empty, got %d", st.Len())
	}
	if !st.cursorCopy().Time.Equal(curBefore.Time) {
		t.Fatal("clear must keep the cursor so the forward tail continues")
	}
}

// --- nil-pool path ----------------------------------------------------------

// TestPane_NilPool is the gate's nil-pool tolerance: with no database the pane renders the
// "no database configured" state, builds no store, issues no Init Cmd, and never panics.
func TestPane_NilPool(t *testing.T) {
	p := newTestPane(t, fullConfig())
	if p.store != nil {
		t.Fatal("nil pool should yield a nil store")
	}
	if cmd := p.Init(); cmd != nil {
		t.Fatal("nil-pool Init must issue no Cmd (nothing to tail)")
	}
	out := p.View()
	if !strings.Contains(out, "no database configured") {
		t.Fatalf("nil-pool View should render the no-DB state, got:\n%s", out)
	}
	// These must not panic with a nil store.
	_ = p.Meta()
	p.Update(logRowsMsg{})
	p.Update(logErrMsg{err: errors.New("transient")})
	p.Update(logBackfillMsg{done: true})
	// A focused key in the no-DB state is harmless.
	if _, cmd := p.Update(tea.KeyPressMsg{}); cmd != nil {
		t.Fatal("nil-pool key press must issue no Cmd")
	}
}

// TestCycleSingle covers the all→item→…→all rotation used by source_cycle / jump_serial.
func TestCycleSingle(t *testing.T) {
	items := []string{"telemetry", "ota-snapshot"}
	step1 := cycleSingle(items, nil)   // all → telemetry
	step2 := cycleSingle(items, step1) // telemetry → ota-snapshot
	step3 := cycleSingle(items, step2) // ota-snapshot → all
	if !strsEqual(step1, []string{"telemetry"}) {
		t.Fatalf("step1 = %q, want [telemetry]", step1)
	}
	if !strsEqual(step2, []string{"ota-snapshot"}) {
		t.Fatalf("step2 = %q, want [ota-snapshot]", step2)
	}
	if len(step3) != 0 {
		t.Fatalf("step3 = %q, want [] (all)", step3)
	}
}

// --- DB-backed live tail (skips without PICPAK_OPS_TEST_DSN) -----------------

// testPool connects to the test DB named by PICPAK_OPS_TEST_DSN and skips when unset
// (air-gap: the DSN/host/secret is never a code literal — it lives only in the env).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PICPAK_OPS_TEST_DSN")
	if dsn == "" {
		t.Skip("PICPAK_OPS_TEST_DSN not set; skipping logs DB tests (no database needed to build)")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test DB: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test DB: %v", err)
	}
	return pool
}

// TestStore_LiveTail_RealDB primes the store against the real logs hypertable and asserts
// it tailed rows and split a multi-segment payload into its lines.
func TestStore_LiveTail_RealDB(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := newLogStore(ctx, pool, "logs:test", nil, configFrom(fullConfig()))
	if err := st.prime(); err != nil {
		t.Fatalf("prime against the real logs table: %v", err)
	}
	lines := st.Snapshot()
	if len(lines) == 0 {
		t.Fatal("expected log lines from the seeded test DB")
	}
	t.Logf("live-tailed %d split line(s) from the real logs table", len(lines))

	texts := map[string]bool{}
	for _, l := range lines {
		texts[l.Text] = true
	}
	// The seeded DB carries the payload "boot42|wifi:connected|got ip" → 3 lines.
	for _, want := range []string{"boot42", "wifi:connected", "got ip"} {
		if !texts[want] {
			t.Errorf("live tail missing the split line %q (have %d lines: %v)", want, len(lines), texts)
		}
	}

	// A second forward poll with no new rows must not error and must add nothing new.
	before := st.Len()
	if err := st.forward(); err != nil {
		t.Fatalf("forward poll: %v", err)
	}
	if st.Len() != before {
		t.Fatalf("forward poll re-added rows (de-dup failed): %d → %d", before, st.Len())
	}
}

// TestStore_BackgroundGoroutine_RealDB drives the actual self-pumping goroutine + Sender
// against the real DB and proves it tails while emitting addressed messages (the -race
// path: a background goroutine appends to the ring while Len() reads it).
func TestStore_BackgroundGoroutine_RealDB(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	var emitted int
	sender := func(tea.Msg) { mu.Lock(); emitted++; mu.Unlock() }

	st := newLogStore(ctx, pool, "logs:test", sender, configFrom(fullConfig()))
	st.Start()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.Len() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	got := st.Len()
	cancel() // stop the goroutine
	time.Sleep(60 * time.Millisecond)

	if got == 0 {
		t.Fatal("background goroutine tailed no lines from the real DB")
	}
	mu.Lock()
	e := emitted
	mu.Unlock()
	if e == 0 {
		t.Fatal("store emitted no addressed messages while tailing")
	}
	t.Logf("background goroutine tailed %d line(s), emitted %d message(s)", got, e)
}
