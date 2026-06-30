package logquery

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB-backed query properties (skipped unless TEST_DATABASE_URL is set; run in the e2e gate). Each test
// inserts rows/fragments DIRECTLY (these probe the read side, not reassembly) and states its red.

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func i64(v int64) *int64 { return &v }

func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — DB property tests skipped")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, stmt := range []string{
		`TRUNCATE logs`,
		`DELETE FROM devices`, // cascades device_log_fragment + device_log_cursor (FK)
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("reset %q: %v", stmt, err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func insLog(t *testing.T, pool *pgxpool.Pool, at time.Time, serial string, bc, seq *int64, payload, source string, gap, suspect bool) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO logs (time, serial, boot_count, seq, offset_start, payload, source, gap, suspect)
		 VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8)`,
		at, serial, bc, seq, payload, source, gap, suspect)
}

func seedDevice(t *testing.T, pool *pgxpool.Pool, serial string) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO devices (serial) VALUES ($1) ON CONFLICT DO NOTHING`, serial)
}

func insFrag(t *testing.T, pool *pgxpool.Pool, serial string, epoch int64, startOff *int64, endOff int64, payload string, gap bool) {
	t.Helper()
	mustExec(t, pool,
		`INSERT INTO device_log_fragment (serial, epoch, start_off, end_off, payload, gap) VALUES ($1,$2,$3,$4,$5,$6)`,
		serial, epoch, startOff, endOff, payload, gap)
}

func texts(p Page) []string {
	out := make([]string, len(p.Lines))
	for i, l := range p.Lines {
		out[i] = l.Text
	}
	return out
}

// Q1 — keyset, not OFFSET. Paging with before= returns strictly older, non-overlapping pages and is
// stable under a concurrent head insert. Red: OFFSET paging → the head insert shifts the window → a line
// is shown twice or skipped.
func TestQuery_KeysetNotOffset_Q1(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		insLog(t, pool, base.Add(time.Duration(i)*time.Second), "S", i64(1), i64(i), fmt.Sprintf("line%d|", i), "telemetry", false, false)
	}
	p1, err := Query(ctx, pool, Params{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(p1); !reflect.DeepEqual(got, []string{"line5", "line4"}) {
		t.Fatalf("page1=%v, want [line5 line4]", got)
	}
	if p1.ReachedEdge || p1.NextCursor == "" {
		t.Fatalf("page1 reached_edge=%v cursor=%q, want more", p1.ReachedEdge, p1.NextCursor)
	}
	// a NEW head row arrives between pages — an OFFSET window would now repeat/skip
	insLog(t, pool, base.Add(6*time.Second), "S", i64(1), i64(6), "line6|", "telemetry", false, false)

	p2, err := Query(ctx, pool, Params{Limit: 2, Before: p1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(p2); !reflect.DeepEqual(got, []string{"line3", "line2"}) {
		t.Fatalf("page2=%v, want [line3 line2] (strictly older, head insert absent)", got)
	}
	for _, l := range p2.Lines {
		if l.Text == "line6" {
			t.Fatal("the concurrent head insert leaked into an older keyset page")
		}
	}
}

// Q2a — total-order tiebreak across devices. Two devices with identical time+seq page deterministically
// because the cursor keys on serial too. Red: (time,seq) only → an ambiguous boundary drops/repeats a row.
func TestQuery_MultiDeviceTiebreak_Q2a(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	ts := base.Add(time.Second)
	insLog(t, pool, ts, "A", i64(1), i64(10), "a|", "telemetry", false, false)
	insLog(t, pool, ts, "B", i64(1), i64(10), "b|", "telemetry", false, false)

	p1, _ := Query(ctx, pool, Params{Limit: 1})
	if len(p1.Lines) != 1 || p1.Lines[0].Serial != "B" { // serial DESC → B first
		t.Fatalf("page1 serial=%v, want B", texts(p1))
	}
	p2, _ := Query(ctx, pool, Params{Limit: 1, Before: p1.NextCursor})
	if len(p2.Lines) != 1 || p2.Lines[0].Serial != "A" {
		t.Fatalf("page2 serial=%v, want A", texts(p2))
	}
	// A is the last row → page2 itself signals the edge (limit+1 saw no further row); there is no page3.
	if !p2.ReachedEdge {
		t.Fatalf("page2 reached_edge=false, want true (A is the final row, B already paged)")
	}
}

// Q2b — NULL-seq window. A cluster of seq=NULL ota-snapshot rows at one (time,serial) collapses to
// COALESCE(seq,0)=0; ctid is the secondary tiebreak so every row pages exactly once. Red: a raw (…,seq)
// tuple compare against NULL drops every such row; or no ctid → the second page is empty and rows vanish.
func TestQuery_NullSeqWindow_Q2b(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	ts := base.Add(time.Second)
	for i := 0; i < 3; i++ {
		insLog(t, pool, ts, "S", nil, nil, fmt.Sprintf("snap%d|", i), "ota-snapshot", false, false)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 6; page++ {
		p, err := Query(ctx, pool, Params{Limit: 1, Before: cursor, Sources: []string{"ota-snapshot"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Lines) == 0 {
			break
		}
		seen[p.Lines[0].Text] = true
		if p.ReachedEdge || p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("paged %d distinct NULL-seq rows, want 3 (ctid tiebreak)", len(seen))
	}
}

// Q3 — splitPayload golden (pure Go). Trailing separator dropped; empty middle kept; no-sep + empty +
// custom sep handled. Red: naive Split keeps the trailing empty token → a blank line per row.
func TestSplitPayload_Q3(t *testing.T) {
	cases := []struct {
		in, sep string
		want    []string
	}{
		{"a|b|c|", "|", []string{"a", "b", "c"}},
		{"a|b|c", "|", []string{"a", "b", "c"}},
		{"", "|", nil},
		{"abc", "|", []string{"abc"}},
		{"a||b", "|", []string{"a", "", "b"}}, // empty MIDDLE token is a genuine blank line
		{"x::y::", "::", []string{"x", "y"}},   // custom separator
		{"|", "|", []string{""}},               // a lone separator → one trailing-dropped empty line
	}
	for _, c := range cases {
		if got := SplitPayload(c.in, c.sep); !reflect.DeepEqual(got, c.want) {
			t.Errorf("SplitPayload(%q,%q)=%v, want %v", c.in, c.sep, got, c.want)
		}
	}
}

// Q4 — span-merge reconstruct shows an overlap exactly once. Two fragments start1<start2<end1 → the
// overlap region appears once. Red: blind ORDER BY start_off + concat → doubled bytes.
func TestReconstruct_OverlapOnce_Q4(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	seedDevice(t, pool, "S")
	// byte-aligned payloads (len == end-start) so the overlap slice is exact
	insFrag(t, pool, "S", 1, i64(0), 10, "0123456789", false)
	insFrag(t, pool, "S", 1, i64(5), 15, "56789ABCDE", false)

	rec, err := Reconstruct(ctx, pool, "S", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "0123456789ABCDE" {
		t.Fatalf("text=%q, want %q (overlap region once)", rec.Text, "0123456789ABCDE")
	}
	if len(rec.Gaps) != 0 {
		t.Fatalf("gaps=%v, want none (contiguous overlap, no discontinuity)", rec.Gaps)
	}
}

// Reconstruct gap divider: a gap=true / NULL-start fragment emits a divider (recorded in gaps[]) then its
// full payload; the plaintext carries only the real bytes. Red: the divider is lost or doubles bytes.
func TestReconstruct_GapDivider(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	seedDevice(t, pool, "S")
	insFrag(t, pool, "S", 2, i64(0), 5, "01234", false)
	insFrag(t, pool, "S", 2, nil, 10, "ABCDE", true) // gap, start_off NULL → sorts by end_off (10)

	rec, err := Reconstruct(ctx, pool, "S", 2)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Text != "01234ABCDE" {
		t.Fatalf("text=%q, want 01234ABCDE", rec.Text)
	}
	if len(rec.Gaps) != 1 || rec.Gaps[0].AfterOff != 5 {
		t.Fatalf("gaps=%v, want [{after_off:5}]", rec.Gaps)
	}
}

// First-line-only gap/suspect: a multi-line row marks only its first Line (one divider per boundary), and
// boot_count rides every line (the SPA's per-line transition heuristic). Red: gap on every split line →
// the SPA draws N dividers for one gapped row.
func TestQuery_GapOnFirstLineOnly(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	insLog(t, pool, base.Add(time.Second), "S", i64(7), i64(1), "x|y|z|", "telemetry", true, true)

	p, err := Query(ctx, pool, Params{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Lines) != 3 {
		t.Fatalf("lines=%d, want 3", len(p.Lines))
	}
	if !p.Lines[0].Gap || !p.Lines[0].Suspect {
		t.Fatal("first line should carry gap+suspect")
	}
	for i := 1; i < 3; i++ {
		if p.Lines[i].Gap || p.Lines[i].Suspect {
			t.Fatalf("continuation line %d carries gap/suspect — would draw a duplicate divider", i)
		}
	}
	for i, l := range p.Lines {
		if l.BootCount == nil || *l.BootCount != 7 {
			t.Fatalf("line %d boot_count=%v, want 7 on every line", i, l.BootCount)
		}
	}
}
