// Package commandstore enqueues Berry commands for the C2 channel. Every enqueue is attributed to the
// operator who issued it (operator_key_id, D17.9 / SEC-M2) — the cheap forensic stamp before the fuller
// operator_audit table lands.
package commandstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScriptMax is the fail-closed C2 script cap in BYTES (octets). The firmware fetches the script into
// malloc(C2_RESP_MAX) with C2_RESP_MAX = 8192 (cmd.c:140) and caps the body at C2_RESP_MAX-1 = 8191 to
// leave room for the NUL terminator (cmd.c:363, buf[len]='\0'); a longer response overflows net.c's fetch
// buffer -> the device fails WITHOUT an ack -> a wedged C2 queue, fleet-wide (K3 poison-pill). 8191 is the
// exact FW-accepted maximum. Byte-measured: len(string) is octets, mirroring the 0017 octet_length CHECK
// (a multibyte script under the old 16384-codepoint char_length cap could still overflow the buffer).
const ScriptMax = 8191

var (
	ErrScriptEmpty   = errors.New("script is empty")
	ErrScriptTooLong = errors.New("script exceeds the 8191-byte cap")
	ErrUnknownSerial = errors.New("target serial is not a registered device")
)

// execer is satisfied by both *pgxpool.Pool and pgx.Tx — MarkApplied runs on the C2 serve tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Enqueue appends a Berry command for one device or the whole fleet ('*'), stamped with the issuing
// operator. A non-'*' serial must already be registered — command_queue has no FK, so an unknown serial
// would otherwise queue a phantom command no device ever polls. Returns the global seq.
//
// idempotencyKey (nil = no dedup) guards against the fleet double-effect (K4): a double-clicked "apply to
// fleet" would otherwise enqueue two identical '*' rows -> every device runs the snippet twice (a doubled
// net_clear/reboot fleet-wide). With a key, an identical STILL-PENDING command dedups to the existing row
// via the command_queue_pending_dedup partial-UNIQUE index (idempotent: the existing seq is returned, no
// error). Once the target(s) applied it (MarkApplied on ack), an identical command may enqueue again.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, serial, script string, note *string, operatorKeyID int64, idempotencyKey *string) (int64, error) {
	switch {
	case script == "":
		return 0, ErrScriptEmpty
	case len(script) > ScriptMax:
		return 0, ErrScriptTooLong
	}
	if serial != "*" {
		var ok bool
		err := pool.QueryRow(ctx, `SELECT true FROM devices WHERE serial = $1`, serial).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrUnknownSerial
		}
		if err != nil {
			return 0, err
		}
	}
	if idempotencyKey != nil && *idempotencyKey != "" {
		var seq int64
		// The partial index is the ON CONFLICT arbiter: a conflict means an identical pending command is
		// already queued. DO NOTHING returns no row -> we read the existing seq back (idempotent no-op).
		err := pool.QueryRow(ctx,
			`INSERT INTO command_queue (serial, script, note, operator_key_id, idempotency_key)
			   VALUES ($1, $2, $3, $4, $5)
			   ON CONFLICT (serial, idempotency_key) WHERE idempotency_key IS NOT NULL AND NOT applied
			   DO NOTHING
			 RETURNING seq`,
			serial, script, note, operatorKeyID, *idempotencyKey).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			err = pool.QueryRow(ctx,
				`SELECT seq FROM command_queue
				   WHERE serial = $1 AND idempotency_key = $2 AND NOT applied
				   ORDER BY seq LIMIT 1`,
				serial, *idempotencyKey).Scan(&seq)
		}
		return seq, err
	}
	var seq int64
	err := pool.QueryRow(ctx,
		`INSERT INTO command_queue (serial, script, note, operator_key_id)
		   VALUES ($1, $2, $3, $4) RETURNING seq`,
		serial, script, note, operatorKeyID).Scan(&seq)
	return seq, err
}

// MarkApplied flips the dedup-applied flag on this serial's keyed commands up to uptoSeq — the C2 serve
// handler calls it after advancing the device cursor (the ack proves the device applied them). An applied
// row leaves command_queue_pending_dedup, so an identical command may be enqueued again. It NEVER touches
// '*' rows (a fleet command is applied per device; one device's ack must not clear the fleet-wide dedup)
// -> a '*' key stays deduped while any device still lags. Best-effort: a failed flip only defers re-entry.
func MarkApplied(ctx context.Context, db execer, serial string, uptoSeq int64) error {
	_, err := db.Exec(ctx,
		`UPDATE command_queue SET applied = true
		   WHERE serial = $1 AND seq <= $2 AND idempotency_key IS NOT NULL AND NOT applied`,
		serial, uptoSeq)
	return err
}
