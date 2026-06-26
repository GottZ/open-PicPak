package logs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The exact SQL, isolated here as the single audit point: every statement is a SELECT
// (this axis is read-only and the shared pool is read_only-enforced, so a stray write is
// rejected with SQLSTATE 25006 anyway). Column names/order match 0001_init.up.sql
// (logs hypertable) exactly. No UI imports here.

// logColumns is the verbatim schema column list, in schema order.
const logColumns = `time, serial, boot_count, seq, offset_start, payload, source, gap, suspect`

// The serial/source filters are passed as text[]; cardinality()=0 (an empty array) means
// "no predicate", which lets the planner use the time index for the all-devices case.

// recentSQL primes the ring with the newest page within the active filter. It walks
// logs_serial_time (or logs_time_idx for the all-devices case) and returns newest-first;
// the store reverses it to chronological.
const recentSQL = `SELECT ` + logColumns + ` FROM logs
WHERE (cardinality($1::text[]) = 0 OR serial = ANY($1))
  AND (cardinality($2::text[]) = 0 OR source = ANY($2))
ORDER BY time DESC, seq DESC NULLS LAST, offset_start DESC NULLS LAST
LIMIT $3`

// forwardSQL is the live-tail forward poll: everything at-or-after the high-water time,
// ascending. `time >= cursor` (inclusive) re-reads the high-water second so a same-second
// row that lands after the previous poll is not missed; the store's de-dup set drops the
// exact rows already ingested at that second. This degradation handles NULL seq (design
// Q3); when seq is later populated the (time,seq) ordering tiebreaks deterministically.
const forwardSQL = `SELECT ` + logColumns + ` FROM logs
WHERE (cardinality($1::text[]) = 0 OR serial = ANY($1))
  AND (cardinality($2::text[]) = 0 OR source = ANY($2))
  AND time >= $3
ORDER BY time ASC, seq ASC NULLS FIRST, offset_start ASC NULLS FIRST
LIMIT $4`

// backfillSQL is the scrollback page: rows strictly older than the oldest loaded time,
// newest-first, capped at a page. The store reverses it and prepends.
const backfillSQL = `SELECT ` + logColumns + ` FROM logs
WHERE (cardinality($1::text[]) = 0 OR serial = ANY($1))
  AND (cardinality($2::text[]) = 0 OR source = ANY($2))
  AND time < $3
ORDER BY time DESC, seq DESC NULLS LAST, offset_start DESC NULLS LAST
LIMIT $4`

// distinctSerialsSQL lists the serials present in `logs` for the device-filter picker.
const distinctSerialsSQL = `SELECT DISTINCT serial FROM logs ORDER BY serial`

// queryRecent reads the newest page for the filter and returns it in CHRONOLOGICAL order
// (oldest first), the shape the store's forward ingest expects.
func queryRecent(ctx context.Context, pool *pgxpool.Pool, f Filter, limit int) ([]logRow, error) {
	rows, err := scanLogRows(ctx, pool, recentSQL, serialArg(f.Serials), serialArg(f.Sources), limit)
	if err != nil {
		return nil, err
	}
	reverse(rows)
	return rows, nil
}

// queryForward reads rows at-or-after the cursor time, already chronological.
func queryForward(ctx context.Context, pool *pgxpool.Pool, f Filter, since time.Time, limit int) ([]logRow, error) {
	return scanLogRows(ctx, pool, forwardSQL, serialArg(f.Serials), serialArg(f.Sources), since, limit)
}

// queryBackfill reads a page older than `before` and returns it CHRONOLOGICAL (oldest
// first) so the store can prepend a contiguous block.
func queryBackfill(ctx context.Context, pool *pgxpool.Pool, f Filter, before time.Time, limit int) ([]logRow, error) {
	rows, err := scanLogRows(ctx, pool, backfillSQL, serialArg(f.Serials), serialArg(f.Sources), before, limit)
	if err != nil {
		return nil, err
	}
	reverse(rows)
	return rows, nil
}

// queryDistinctSerials lists the serials seen in the hypertable.
func queryDistinctSerials(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, distinctSerialsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// scanLogRows runs one of the log SELECTs and scans the fixed column shape.
func scanLogRows(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]logRow, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []logRow
	for rows.Next() {
		var r logRow
		if err := rows.Scan(
			&r.Time, &r.Serial, &r.BootCount, &r.Seq, &r.OffsetStart,
			&r.Payload, &r.Source, &r.Gap, &r.Suspect,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// serialArg normalizes a filter slice for pgx: a nil slice becomes a non-nil empty slice
// so $n::text[] is always a valid (empty) array, making cardinality()=0 the "all" case.
func serialArg(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// reverse flips a row slice in place (DESC query result → chronological).
func reverse(rows []logRow) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}
