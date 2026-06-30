package commandstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cursor is one device's C2 ack position (device_c2_cursor). The SSE c2cursor producer (Design 23 §4.5 /
// D23.7) diffs the snapshot on applied_seq ONLY; last_poll_at rides the payload for display but never
// triggers a delta (a poll that only bumps last_poll_at must not churn the stream — T9).
type Cursor struct {
	Serial     string     `json:"serial"`
	AppliedSeq int64      `json:"applied_seq"`
	LastPollAt *time.Time `json:"last_poll_at"`
}

// CursorSnapshot reads every device's current C2 cursor — the source for the SSE poll-and-diff producer.
func CursorSnapshot(ctx context.Context, pool *pgxpool.Pool) ([]Cursor, error) {
	rows, err := pool.Query(ctx, `SELECT serial, applied_seq, last_poll_at FROM device_c2_cursor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cursor{}
	for rows.Next() {
		var c Cursor
		if err := rows.Scan(&c.Serial, &c.AppliedSeq, &c.LastPollAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentCommand is one recently-enqueued command with its per-target applied/pending breakdown — the
// rehydration source the editor seeds from on mount (D23.9), after which the c2cursor SSE carries live
// deltas. The script is DELIBERATELY omitted: the feedback view needs seq/target/note, never the payload,
// and the script can carry URLs/SSIDs (air-gap, T11). "applied" here means the cursor crossed the seq =
// delivered + attempted, NEVER "succeeded" (D23.4 — no device→server result channel).
type RecentCommand struct {
	Seq           int64     `json:"seq"`
	Serial        string    `json:"serial"`
	Note          *string   `json:"note"`
	CreatedAt     time.Time `json:"created_at"`
	OperatorKeyID *int64    `json:"operator_key_id"`
	TargetCount   int       `json:"target_count"`
	AppliedCount  int       `json:"applied_count"`
}

// RecentCommands returns recent command_queue rows (within window, newest first, capped) joined to the
// device cursors for a per-command applied/target breakdown: a single-serial command is applied when that
// device's cursor reached its seq (target=1); a '*' command is the N-of-M across all device cursors
// (target = current roster size; a device onboarded after the seq seeds its cursor past it → counted
// applied, moot, D23.6/D17.7). Read-only — stays reachable under read-only degradation (D23.9).
func RecentCommands(ctx context.Context, pool *pgxpool.Pool, window time.Duration, limit int) ([]RecentCommand, error) {
	const q = `
SELECT cq.seq, cq.serial, cq.note, cq.created_at, cq.operator_key_id,
  CASE WHEN cq.serial = '*' THEN (SELECT count(*)::int FROM devices)
       ELSE 1 END AS target_count,
  CASE WHEN cq.serial = '*'
       THEN (SELECT count(*)::int FROM device_c2_cursor c WHERE c.applied_seq >= cq.seq)
       ELSE (SELECT count(*)::int FROM device_c2_cursor c WHERE c.serial = cq.serial AND c.applied_seq >= cq.seq)
  END AS applied_count
FROM command_queue cq
WHERE cq.created_at >= now() - make_interval(secs => $1)
ORDER BY cq.seq DESC
LIMIT $2`
	rows, err := pool.Query(ctx, q, window.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecentCommand{}
	for rows.Next() {
		var rc RecentCommand
		if err := rows.Scan(&rc.Seq, &rc.Serial, &rc.Note, &rc.CreatedAt, &rc.OperatorKeyID, &rc.TargetCount, &rc.AppliedCount); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}
