// Package logquery is the operator-facing read side of the device log subsystem (A21 / Design 21 part b,
// the M5 operator zone). It never imports the ingest reassembly path; the two halves meet only at the
// shared logs / device_log_fragment tables (D21.1). Everything here is read-only (D21.7) and env-free —
// policy (separator, page cap, windows) is passed in by the owning cmd/admin process (mechanism=code,
// policy=data).
//
// Keyset pagination, NEVER OFFSET (D21.11): an OFFSET page shifts under a concurrent head insert and
// repeats/drops a line. The keyset orders + keys on (time, serial, COALESCE(seq,0), ctid): seq is NULL on
// ota-snapshot rows (0001:124) and a raw tuple compare against NULL is UNKNOWN — silently dropping every
// such row — so COALESCE(seq,0) collapses them and ctid is the documented secondary tiebreak for that
// NULL-seq window (many rows collapse to 0 at one (time, serial)).
package logquery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Line is one rendered log line (Doc 09 §4.1 shape). A DB row's payload is split on the separator into
// one Line per token, each carrying the row's metadata. Gap/Suspect are set ONLY on a row's FIRST line
// (the boundary) so the SPA draws exactly one divider per gapped/suspect row (D21.10); continuation lines
// carry false. Synthetic is always false from the server — the SPA builds its own Synthetic divider Lines.
type Line struct {
	Time      time.Time `json:"time"`
	Serial    string    `json:"serial"`
	Seq       *int64    `json:"seq"`        // the ROW seq (nullable on ota-snapshot) — the client's dedup/order key
	Idx       int       `json:"idx"`        // token index within the row — completes the per-line identity
	BootCount *int64    `json:"boot_count"` // nullable (ota-snapshot / absent bc)
	Source    string    `json:"source"`
	Gap       bool      `json:"gap"`
	Suspect   bool      `json:"suspect"`
	Text      string    `json:"text"`
	Synthetic bool      `json:"synthetic"`
}

// Cursor is the opaque keyset bookmark carried in next_cursor (base64url JSON). Seq is COALESCE(seq,0);
// Ctid is the within-chunk physical pointer that disambiguates the NULL-seq cluster — stable for the
// live short-window paging this serves, not a durable bookmark (seq is the durable order).
type Cursor struct {
	Time   time.Time `json:"t"`
	Serial string    `json:"s"`
	Seq    int64     `json:"q"`
	Ctid   string    `json:"c"`
}

// Page is one keyset page. NextCursor is "" when ReachedEdge (the retention floor / no more rows).
type Page struct {
	Lines       []Line `json:"lines"`
	NextCursor  string `json:"next_cursor"`
	ReachedEdge bool   `json:"reached_edge"`
}

// Params drives Query. Serials empty = all devices; Sources defaults to telemetry+ota-snapshot; Boot is
// an optional epoch filter; Before is an opaque cursor ("" = newest); Limit is capped by the caller; Sep
// is LOG_LINE_SEPARATOR.
type Params struct {
	Serials []string
	Sources []string
	Boot    *int64
	Before  string
	Limit   int
	Sep     string
}

// EncodeCursor / DecodeCursor make the keyset bookmark opaque to the client (base64url JSON, no padding).
func EncodeCursor(c Cursor) string {
	b, _ := json.Marshal(c) //nolint:errchkjson // fixed struct, cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

func DecodeCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor decode: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("cursor unmarshal: %w", err)
	}
	return c, nil
}

// SplitPayload splits a row's payload on sep into rendered lines, dropping a SINGLE trailing empty token
// (the ring tail ends with a separator). An empty middle token is kept (a genuine blank log line). An
// empty payload yields no lines. (Doc 09 splitPayload; Q3 golden.)
func SplitPayload(payload, sep string) []string {
	if payload == "" {
		return nil
	}
	parts := strings.Split(payload, sep)
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// dbRow is one scanned logs row before payload-splitting.
type dbRow struct {
	t       time.Time
	serial  string
	boot    *int64
	seq     *int64
	payload string
	source  string
	gap     bool
	suspect bool
	ctid    string
}

func (r dbRow) seqOrZero() int64 {
	if r.seq != nil {
		return *r.seq
	}
	return 0
}

// Query returns one keyset page of reassembled log lines, newest first, paging BACKWARD via p.Before
// (Doc 09 scrollback). It fetches Limit+1 rows to set ReachedEdge precisely, trims to Limit, and the
// next_cursor points at the last RETURNED row (per-row, shared by that row's flattened lines).
func Query(ctx context.Context, pool *pgxpool.Pool, p Params) (Page, error) {
	if p.Limit <= 0 {
		p.Limit = 100
	}
	if len(p.Sources) == 0 {
		p.Sources = []string{"telemetry", "ota-snapshot"}
	}
	// A nil slice encodes as SQL NULL, and cardinality(NULL)=NULL would make the WHERE clause NULL and
	// drop EVERY row. An empty NON-nil slice encodes as '{}' so cardinality('{}')=0 → the all-devices path.
	serials := p.Serials
	if serials == nil {
		serials = []string{}
	}
	sep := p.Sep
	if sep == "" {
		sep = "|"
	}

	// keyset args: NULL curTime short-circuits the tuple compare (first page). Row-value comparison does
	// lexicographic keyset semantics; COALESCE(seq,0) + ctid::tid keep the NULL-seq window total-ordered.
	var curTime *time.Time
	var curSerial, curCtid *string
	var curSeq *int64
	if p.Before != "" {
		c, err := DecodeCursor(p.Before)
		if err != nil {
			return Page{}, err
		}
		curTime, curSerial, curSeq, curCtid = &c.Time, &c.Serial, &c.Seq, &c.Ctid
	}

	const q = `
SELECT time, serial, boot_count, seq, payload, source, gap, suspect, ctid::text
FROM   logs
WHERE  (cardinality($1::text[]) = 0 OR serial = ANY($1))
  AND  source = ANY($2::text[])
  AND  ($3::bigint IS NULL OR boot_count = $3)
  AND  ($4::timestamptz IS NULL
        OR (time, serial, COALESCE(seq,0), ctid) < ($4, $5, $6, $7::tid))
ORDER BY time DESC, serial DESC, COALESCE(seq,0) DESC, ctid DESC
LIMIT  $8`

	rows, err := pool.Query(ctx, q, serials, p.Sources, p.Boot, curTime, curSerial, curSeq, curCtid, p.Limit+1)
	if err != nil {
		return Page{}, fmt.Errorf("log query: %w", err)
	}
	defer rows.Close()

	var recs []dbRow
	for rows.Next() {
		var r dbRow
		if err := rows.Scan(&r.t, &r.serial, &r.boot, &r.seq, &r.payload, &r.source, &r.gap, &r.suspect, &r.ctid); err != nil {
			return Page{}, fmt.Errorf("log scan: %w", err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("log rows: %w", err)
	}

	hasMore := len(recs) > p.Limit
	if hasMore {
		recs = recs[:p.Limit]
	}

	page := Page{Lines: []Line{}, ReachedEdge: !hasMore}
	for _, r := range recs {
		for i, text := range SplitPayload(r.payload, sep) {
			ln := Line{Time: r.t, Serial: r.serial, Seq: r.seq, Idx: i, BootCount: r.boot, Source: r.source, Text: text}
			if i == 0 { // gap/suspect mark only the row's first line — one divider per boundary
				ln.Gap, ln.Suspect = r.gap, r.suspect
			}
			page.Lines = append(page.Lines, ln)
		}
	}
	if hasMore && len(recs) > 0 {
		last := recs[len(recs)-1]
		page.NextCursor = EncodeCursor(Cursor{Time: last.t, Serial: last.serial, Seq: last.seqOrZero(), Ctid: last.ctid})
	}
	return page, nil
}

// DistinctSerials lists the serials with logs in the recent window (the filter-picker source). The time
// bound gives hypertable chunk exclusion; window is a policy duration from the caller.
func DistinctSerials(ctx context.Context, pool *pgxpool.Pool, window time.Duration) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT serial FROM logs WHERE time > now() - make_interval(secs => $1) ORDER BY serial`,
		window.Seconds())
	if err != nil {
		return nil, fmt.Errorf("serials query: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("serials scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GapMark records an offset after which a reconstruct divider falls (gap=true fragment or a start_off
// discontinuity). The viewer renders the divider; the plaintext Text carries only the real bytes.
type GapMark struct {
	AfterOff int64 `json:"after_off"`
}

// Reconstruction is the gapless plaintext rebuild of one (serial, epoch) stream segment.
type Reconstruction struct {
	Serial string    `json:"serial"`
	Epoch  int64     `json:"epoch"`
	Text   string    `json:"text"`
	Gaps   []GapMark `json:"gaps"`
}

type fragment struct {
	startOff *int64
	endOff   int64
	payload  string
	gap      bool
}

func (f fragment) sortKey() int64 { // merge invariant is over the START position (Doc 04 §5.12)
	if f.startOff != nil {
		return *f.startOff
	}
	return f.endOff // a NULL start (gap/epoch-start) sorts by its end
}

// Reconstruct rebuilds one stream segment via span-merge (§4.4): fetch the fragments for ONE (serial,
// epoch) — riding device_log_fragment_serial_epoch (0001:76) — sort by start position in memory (the
// built index keys on end_off, but the merge is defined over start_off, so end_off ordering would
// mis-merge unequal-length fragments), then emit each byte range exactly once even if an overrun re-push
// slid start_off into an overlap.
//
// NB: the overlap slice treats a fragment's payload as byte-aligned with its [start_off, end_off) raw
// range. This is a reconstruct-only, viewer-cosmetic assumption — it is NEVER used for the cursor ack
// (D21.2 forbids assuming transport length == raw bytes). The common case is a contiguous tiling where
// start == prev_end, so no slicing happens at all; the slice matters only for the rare overrun overlap.
func Reconstruct(ctx context.Context, pool *pgxpool.Pool, serial string, epoch int64) (Reconstruction, error) {
	rows, err := pool.Query(ctx,
		`SELECT start_off, end_off, payload, gap FROM device_log_fragment WHERE serial=$1 AND epoch=$2`,
		serial, epoch)
	if err != nil {
		return Reconstruction{}, fmt.Errorf("fragment query: %w", err)
	}
	defer rows.Close()
	var frags []fragment
	for rows.Next() {
		var f fragment
		if err := rows.Scan(&f.startOff, &f.endOff, &f.payload, &f.gap); err != nil {
			return Reconstruction{}, fmt.Errorf("fragment scan: %w", err)
		}
		frags = append(frags, f)
	}
	if err := rows.Err(); err != nil {
		return Reconstruction{}, fmt.Errorf("fragment rows: %w", err)
	}

	sort.Slice(frags, func(i, j int) bool {
		ki, kj := frags[i].sortKey(), frags[j].sortKey()
		if ki != kj {
			return ki < kj
		}
		return frags[i].endOff < frags[j].endOff
	})

	var b strings.Builder
	gaps := []GapMark{}
	var prevEnd int64
	started := false
	for _, f := range frags {
		switch {
		case !started:
			// first emitted fragment: no preceding content → no divider, take it whole
			b.WriteString(f.payload)
			prevEnd = f.endOff
			started = true
		case f.gap || f.startOff == nil:
			// gap / epoch-start fragment → a divider, then the full payload (no known base to dedup)
			gaps = append(gaps, GapMark{AfterOff: prevEnd})
			b.WriteString(f.payload)
			prevEnd = f.endOff
		case *f.startOff > prevEnd:
			// a contiguous fragment that starts past prev_end → a real discontinuity → divider first
			gaps = append(gaps, GapMark{AfterOff: prevEnd})
			b.WriteString(f.payload)
			prevEnd = f.endOff
		default:
			// overlap or perfectly contiguous: emit only the suffix beyond prev_end (skip covered bytes)
			skip := prevEnd - *f.startOff // == max(prev_end,start) - start; >=0 here
			if skip < int64(len(f.payload)) {
				b.WriteString(f.payload[skip:])
			}
			if f.endOff > prevEnd {
				prevEnd = f.endOff
			}
		}
	}
	return Reconstruction{Serial: serial, Epoch: epoch, Text: b.String(), Gaps: gaps}, nil
}

// PruneFragments deletes fragments older than ttl (§4.7 — the fragment table is a plain table with no
// retention policy; this keeps reassembly from growing it unbounded). Doc 04 F4-W5 owns the canonical
// retention/Grafana work; this is the minimal day-one prune. Returns the rows removed.
func PruneFragments(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		`DELETE FROM device_log_fragment WHERE created_at < now() - make_interval(secs => $1)`,
		ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("fragment prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---- live tail (the SSE `log` producer's data side, A21 W3 / Design 21 §4.5) ----

// HighWater is the forward poll-and-diff cursor the SSE producer advances each hub tick (D21.12). Set is
// false until the first prime — a cold producer starts from the newest existing row (or DB now() on an
// empty table) and emits ONLY rows that arrive AFTER, so the SSE is a live tail (history comes from the
// REST keyset query, §4.3); the DB is the source of truth, SSE the accelerator.
type HighWater struct {
	Time   time.Time
	Serial string
	Seq    int64 // COALESCE(seq,0)
	Ctid   string
	Set    bool
}

// TailEvent is one new log row for the SSE `log` payload (Design 21 §4.5). Lines is the server-split
// payload; the hub json.Marshal's this whole value (frame integrity, Q6). seq/boot_count are nullable.
type TailEvent struct {
	Serial    string    `json:"serial"`
	Time      time.Time `json:"time"`
	Seq       *int64    `json:"seq"`
	BootCount *int64    `json:"boot_count"`
	Source    string    `json:"source"`
	Gap       bool      `json:"gap"`
	Suspect   bool      `json:"suspect"`
	Lines     []string  `json:"lines"`
}

// Tail returns log rows strictly NEWER than hw (forward keyset, ascending so the client appends in
// order), advancing and returning the high-water. On a cold hw (Set==false) it PRIMES — sets the
// high-water to the newest existing row (or DB now() if empty) and returns NO events — so a fresh
// producer never floods the recent window; only rows arriving after the prime are emitted. `time >=
// hw.Time` gives hypertable chunk exclusion (the keyset alone would scan from any chunk). batch caps one
// tick; the remainder rides the next tick (hw advances to the last emitted row, no loss).
func Tail(ctx context.Context, pool *pgxpool.Pool, hw HighWater, sep string, batch int) ([]TailEvent, HighWater, error) {
	if sep == "" {
		sep = "|"
	}
	if batch <= 0 {
		batch = 200
	}
	if !hw.Set {
		var nhw HighWater
		err := pool.QueryRow(ctx,
			`SELECT time, serial, COALESCE(seq,0), ctid::text
			 FROM logs ORDER BY time DESC, serial DESC, COALESCE(seq,0) DESC, ctid DESC LIMIT 1`).
			Scan(&nhw.Time, &nhw.Serial, &nhw.Seq, &nhw.Ctid)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// empty table: prime at the DB clock with a zero baseline → emit only future arrivals
			if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&nhw.Time); err != nil {
				return nil, hw, fmt.Errorf("tail prime now: %w", err)
			}
			nhw.Serial, nhw.Seq, nhw.Ctid = "", 0, "(0,0)"
		case err != nil:
			return nil, hw, fmt.Errorf("tail prime: %w", err)
		}
		nhw.Set = true
		return nil, nhw, nil
	}

	rows, err := pool.Query(ctx,
		`SELECT time, serial, boot_count, seq, payload, source, gap, suspect, ctid::text
		 FROM logs
		 WHERE time >= $1
		   AND (time, serial, COALESCE(seq,0), ctid) > ($1, $2, $3, $4::tid)
		 ORDER BY time ASC, serial ASC, COALESCE(seq,0) ASC, ctid ASC
		 LIMIT $5`,
		hw.Time, hw.Serial, hw.Seq, hw.Ctid, batch)
	if err != nil {
		return nil, hw, fmt.Errorf("tail query: %w", err)
	}
	defer rows.Close()

	out := []TailEvent{}
	nhw := hw
	for rows.Next() {
		var r dbRow
		if err := rows.Scan(&r.t, &r.serial, &r.boot, &r.seq, &r.payload, &r.source, &r.gap, &r.suspect, &r.ctid); err != nil {
			return nil, hw, fmt.Errorf("tail scan: %w", err)
		}
		out = append(out, TailEvent{
			Serial: r.serial, Time: r.t, Seq: r.seq, BootCount: r.boot, Source: r.source,
			Gap: r.gap, Suspect: r.suspect, Lines: SplitPayload(r.payload, sep),
		})
		nhw = HighWater{Time: r.t, Serial: r.serial, Seq: r.seqOrZero(), Ctid: r.ctid, Set: true}
	}
	if err := rows.Err(); err != nil {
		return nil, hw, fmt.Errorf("tail rows: %w", err)
	}
	return out, nhw, nil
}
