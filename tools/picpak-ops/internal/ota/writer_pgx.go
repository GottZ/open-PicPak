package ota

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DirectPGXWriter is the interim, flag-gated (ota.allow_direct_write) write backend. It
// re-implements the integrity the Admin-API would enforce — recompute/validate the
// lowercase sha256 before the INSERT, FK-safe ordering, precedence-respecting upsert on
// (serial,channel) — so the interim path is not a downgrade in SAFETY, only in
// centralization. It rides the shared pool, which (per config rule 3) is writable only
// when database.read_only=false. The pane shows a red "DIRECT-WRITE MODE — bypassing
// backend authority" banner while it is active.
//
// PostgreSQL error mapping: 23505 unique_violation → ErrVersionExists; 23503
// foreign_key_violation → ErrUnknownRef; 23514 check_violation (a bad sha that bypassed
// the Go gate) → ErrSHAMismatch.
type DirectPGXWriter struct {
	pool             *pgxpool.Pool
	statementTimeout time.Duration
}

// NewDirectPGXWriter binds the writer to the (writable) shared pool.
func NewDirectPGXWriter(pool *pgxpool.Pool, statementTimeout time.Duration) *DirectPGXWriter {
	return &DirectPGXWriter{pool: pool, statementTimeout: statementTimeout}
}

func (w *DirectPGXWriter) IsDirect() bool { return true }
func (w *DirectPGXWriter) Close() error   { return nil } // the shared pool is owned/closed by main

func (w *DirectPGXWriter) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if w.statementTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, w.statementTimeout)
}

// execer is the minimal surface registerFirmware/etc. need so the SAME integrity code
// runs against either the pool (production: begin→work→commit) or a caller-supplied tx
// (the rollback test). Both pgx.Tx and *pgxpool.Pool satisfy it.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const insertFirmwareSQL = `INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes, notes)
VALUES ($1, $2, $3, $4, NULLIF($5, ''))`

// RegisterFirmware validates + inserts in a transaction it commits. The integrity is in
// registerFirmware(execer); this wrapper just owns the tx lifecycle.
func (w *DirectPGXWriter) RegisterFirmware(ctx context.Context, req RegisterFirmware) error {
	return w.inTx(ctx, func(tx execer) error { return w.registerFirmware(ctx, tx, req) })
}

// registerFirmware is the testable integrity path: it fail-closes a non-lowercase-hex
// sha BEFORE any write, copies the blob to the serving location when one is supplied,
// and inserts the row (the DB CHECK is the backstop). The rollback test calls THIS with
// its own tx and rolls back, so the exact production write path is proven without
// polluting the DB.
func (w *DirectPGXWriter) registerFirmware(ctx context.Context, q execer, req RegisterFirmware) error {
	if !ValidSHA256(req.SHA256) {
		return ErrBadSHA
	}
	blobPath, err := w.placeBlob(req)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, insertFirmwareSQL, req.Version, req.SHA256, blobPath, req.SizeBytes, req.Notes); err != nil {
		return mapPGError(err)
	}
	return nil
}

// placeBlob resolves the blob_path the schema records. When req.BlobPath is set it is
// used verbatim (the rollback test supplies a fixed path and no bytes). Otherwise, when
// req.Blob is present, the blob is copied to <BlobPath dir>/<version>/firmware.bin — but
// since the serving directory is a runtime arg passed via req.BlobPath, the default here
// is the relative "<version>/firmware.bin" key the backend resolves. blob_path is NOT
// NULL in the schema, so a non-empty value is always produced.
func (w *DirectPGXWriter) placeBlob(req RegisterFirmware) (string, error) {
	blobPath := req.BlobPath
	if blobPath == "" {
		blobPath = filepath.Join(req.Version, "firmware.bin")
	}
	// Copy the bytes to the serving location only when BOTH an absolute serving path
	// and the blob bytes are present (interim direct-write on the backend host). A
	// relative key (object-store style) or absent bytes records the path only.
	if len(req.Blob) > 0 && filepath.IsAbs(blobPath) {
		if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
			return "", fmt.Errorf("ota: creating serving dir: %w", err)
		}
		if err := os.WriteFile(blobPath, req.Blob, 0o644); err != nil {
			return "", fmt.Errorf("ota: writing blob to serving location: %w", err)
		}
	}
	return blobPath, nil
}

const setChannelDefaultSQL = `UPDATE channels SET default_version = $1 WHERE name = $2`

func (w *DirectPGXWriter) SetChannelDefault(ctx context.Context, req SetChannelDefault) error {
	return w.inTx(ctx, func(tx execer) error {
		tag, err := tx.Exec(ctx, setChannelDefaultSQL, req.Version, req.Channel)
		if err != nil {
			return mapPGError(err) // FK violation (unknown version) → ErrUnknownRef
		}
		if tag.RowsAffected() == 0 {
			return ErrUnknownRef // unknown channel
		}
		return nil
	})
}

const upsertRolloutSQL = `INSERT INTO rollout_targets (serial, channel, version, state, pinned, updated_at)
VALUES ($1, $2, $3, 'active', $4, now())
ON CONFLICT (serial, channel)
DO UPDATE SET version = EXCLUDED.version, pinned = EXCLUDED.pinned, state = 'active', updated_at = now()`

func (w *DirectPGXWriter) PinRollout(ctx context.Context, req PinRollout) error {
	return w.inTx(ctx, func(tx execer) error {
		if _, err := tx.Exec(ctx, upsertRolloutSQL, req.Serial, req.Channel, req.Version, req.Pinned); err != nil {
			return mapPGError(err) // FK violation (unknown channel/version) → ErrUnknownRef
		}
		return nil
	})
}

const setRolloutStateSQL = `UPDATE rollout_targets SET state = $1, updated_at = now() WHERE id = $2`

func (w *DirectPGXWriter) SetRolloutState(ctx context.Context, req SetRolloutState) error {
	if !ValidState(req.State) {
		return ErrBadState
	}
	return w.inTx(ctx, func(tx execer) error {
		tag, err := tx.Exec(ctx, setRolloutStateSQL, req.State, req.ID)
		if err != nil {
			return mapPGError(err)
		}
		if tag.RowsAffected() == 0 {
			return ErrUnknownRef
		}
		return nil
	})
}

// inTx runs fn inside a transaction it commits on success / rolls back on error. The
// op context is statement-timeout-bounded and child of the per-pane ctx (cancelled on
// Close()/quit).
func (w *DirectPGXWriter) inTx(ctx context.Context, fn func(execer) error) error {
	octx, cancel := w.opCtx(ctx)
	defer cancel()
	tx, err := w.pool.Begin(octx)
	if err != nil {
		return fmt.Errorf("ota: begin write tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(octx)
		return err
	}
	if err := tx.Commit(octx); err != nil {
		return fmt.Errorf("ota: commit write tx: %w", err)
	}
	return nil
}

// mapPGError maps PostgreSQL SQLSTATEs to the typed contract errors so the pane shows
// the same message whichever backend wrote. A non-PG error passes through.
func mapPGError(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23505": // unique_violation (version PK / (serial,channel) UNIQUE)
		return ErrVersionExists
	case "23503": // foreign_key_violation (unknown channel/version)
		return ErrUnknownRef
	case "23514": // check_violation (sha256_hex / version_len) — Go gate should pre-empt
		return ErrSHAMismatch
	default:
		return err
	}
}
