// Package logs is the background log-viewer pane (axis 09): a read-only, live-tailing
// view of the fleet's exfiltrated device logs in the backend TimescaleDB `logs`
// hypertable, read directly over the shared read-only pgx pool (internal/db, K9).
//
// It implements the canonical pane.Pane (K1, embedding pane.BasePane) and owns a
// logStore — a long-lived goroutine that polls a monotone keyset cursor, parses each
// `payload` ring tail into lines, injects synthetic boot-gap / suspect markers, and
// keeps a bounded in-store ring. The store drives its OWN cadence (poll_interval, K3)
// and emits results as addressed app.PaneMsg through the injected Sender (K2), so it
// keeps tailing while the pane is backgrounded; returning to a hidden pane shows the
// warm ring, not a cold start. The store goroutine lives for the pane's whole life and
// is stopped only by Close() — never on blur.
//
// The single load-bearing parse fact: the device maps '\n'→'|' before exfil
// (firmware logbuf.c), so one `payload` is several lines joined by a separator; the TUI
// splits it back. The separator, the marker templates, the colors, the cadence and the
// bounds are ALL config (Policy=Data) — there is no magic value in this package.
package logs

import "time"

// SyntheticKind classifies a Line that the store injected (not device text): a boot-gap
// divider or a suspect marker. Device content lines are SynthNone.
type SyntheticKind int

const (
	SynthNone    SyntheticKind = iota // a real device log line
	SynthGap                          // boot-epoch / gap divider (template, config)
	SynthSuspect                      // offset-rollback suspect marker (config)
)

// Line is one rendered log line: either a device text line split out of a row's
// `payload`, or a synthetic marker the store injected. All content lines split from one
// row share that row's Time/Serial/BootCount/Source (the ring carries no per-line
// timestamp — the lines are a burst exfiltrated at one fetch; surfaced honestly, never
// faked as distinct times).
type Line struct {
	Time      time.Time
	Serial    string
	BootCount int64
	HasBoot   bool
	Source    string
	Text      string
	Synthetic SyntheticKind
}

// Cursor is the forward live-tail high-water mark. Time is the newest row time ingested.
// Seq is the newest non-NULL seq seen (NULL in the current ingest — see store de-dup,
// design Q3); it is retained for the future strict (time,seq) keyset once reassembly
// (S5f) populates seq, but is not relied on for correctness today.
type Cursor struct {
	Time    time.Time
	Seq     *int64
	HasData bool
}

// Filter is the SQL-side row filter the store applies in its WHERE clause. Empty slices
// mean "no predicate" (all serials / all sources), which lets the planner use the time
// index for the all-devices case (design §8 poll-storm mitigation).
type Filter struct {
	Serials []string // empty = all devices
	Sources []string // empty = all sources
}

// Config is the logs package's own snapshot of the [logs] section (Policy=Data). It is
// mapped from *config.Config in logs.go so the store/parse code never imports config and
// stays headless-testable.
type Config struct {
	LineSeparator   string
	PollInterval    time.Duration
	PollBatch       int
	BackfillPage    int
	LiveRingLines   int
	HistoryMaxRows  int
	DefaultSerials  []string
	DefaultSources  []string
	ShowGapMarkers  bool
	GapMarkerFormat string
	ShowSuspect     bool
	SuspectMarker   string
	FollowDefault   bool
	ErrorBackoff    time.Duration
	ErrorBackoffMax time.Duration
	TimestampFormat string
	Timezone        string
	ColorizeBy      string
	Palette         []string
	QueryTimeout    time.Duration
}

// logRow is the raw scan shape of one `logs` row (column order = schema order). The
// nullable bigint columns (boot_count, seq, offset_start) scan through pointers; seq is
// NULL in the current ingest.
type logRow struct {
	Time        time.Time
	Serial      string
	BootCount   *int64
	Seq         *int64
	OffsetStart *int64
	Payload     string
	Source      string
	Gap         bool
	Suspect     bool
}

// The store→model message taxonomy. The store goroutine returns one of these as the
// Payload of an addressed app.PaneMsg{To:id} (K2); the model folds it in Update and
// re-renders from the store ring. None of these carry pane state — the goroutine never
// touches the pane.

// logRowsMsg announces a completed forward poll (or the initial prime). Added is the
// number of parsed lines appended to the ring; Cur is the advanced cursor. The model
// re-snapshots the ring rather than trusting a lines payload, so a hidden→shown pane is
// always consistent with the durable ring.
type logRowsMsg struct {
	added int
	cur   Cursor
}

// logBackfillMsg announces a completed scrollback page (older rows prepended). Done is
// true when the page was short (retention edge reached) or the history cap was hit.
type logBackfillMsg struct {
	added int
	done  bool
}

// logSerialsMsg carries the distinct serials present in `logs` (for the device-filter
// picker). Refreshed on prime.
type logSerialsMsg struct {
	serials []string
}

// logErrMsg reports a non-fatal store error (query/scan). The pane keeps its last good
// ring and shows a "reconnecting" badge; the store backs off and retries.
type logErrMsg struct {
	err error
}
