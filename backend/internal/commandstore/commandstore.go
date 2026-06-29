// Package commandstore enqueues Berry commands for the C2 channel. Every enqueue is attributed to the
// operator who issued it (operator_key_id, D17.9 / SEC-M2) — the cheap forensic stamp before the fuller
// operator_audit table lands.
package commandstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ScriptMax mirrors the command_queue.command_script_len CHECK and the firmware C2_SCRIPT_MAX cap.
const ScriptMax = 16384

var (
	ErrScriptEmpty   = errors.New("script is empty")
	ErrScriptTooLong = errors.New("script exceeds the 16384-byte cap")
	ErrUnknownSerial = errors.New("target serial is not a registered device")
)

// Enqueue appends a Berry command for one device or the whole fleet ('*'), stamped with the issuing
// operator. A non-'*' serial must already be registered — command_queue has no FK, so an unknown serial
// would otherwise queue a phantom command no device ever polls. Returns the global seq.
func Enqueue(ctx context.Context, pool *pgxpool.Pool, serial, script string, note *string, operatorKeyID int64) (int64, error) {
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
	// TODO(enqueue-dedup): an append-only queue double-executes a retried/double-submitted command
	// (Root A gap-minor). A dedup mechanism (client idempotency key, or a pending-identical guard)
	// needs a schema decision — tracked in the masterplan, not built in A17.
	var seq int64
	err := pool.QueryRow(ctx,
		`INSERT INTO command_queue (serial, script, note, operator_key_id)
		   VALUES ($1, $2, $3, $4) RETURNING seq`,
		serial, script, note, operatorKeyID).Scan(&seq)
	return seq, err
}
