// Package rolloutadmin is the OTA management WRITE path: register a firmware version (blob upload +
// server re-hash), set a channel default, upsert/state/delete a rollout target, and the admin-side
// list reads. It is ADMIN-ONLY (M5/D20.1): cmd/ingest must NEVER import it (build guard T8), so a
// parser-RCE in the public process cannot reach INSERT firmware_versions or blob storage. Reads are
// resolved via the shared internal/rollout (the single resolver, D20.2); this package owns only writes.
package rolloutadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrShaMismatch is returned when the server-recomputed sha256 of the uploaded bytes does not equal the
// client-claimed sha (D20.7). Nothing is persisted — no row, no blob. The handler maps it to 422.
var ErrShaMismatch = errors.New("rolloutadmin: sha256 mismatch (server re-hash != claimed)")

// RegisterFirmware persists a firmware version with the CRASH-SAFE order of §4.4.1: re-hash the bytes
// in memory → reserve the row (so a duplicate version is rejected BEFORE any blob is written) → write
// the blob last. The server-computed sha is authoritative (the client's is advisory, D20.7) and is what
// the FW will later verify fail-closed, so it MUST equal the bytes' real hash. blob_path is content-
// addressed (<sha>.bin): collision-free, filesystem-safe regardless of the version's free-form charset.
//
// A crash AFTER the INSERT but before the blob write leaves a row pointing at a missing blob — the
// serve path's 500+alert (§4.3), an operator reconcile, NOT a security hole; strictly better than the
// reverse order, which could orphan a blob or store a sha the bytes never had. Returns the server sha.
func RegisterFirmware(ctx context.Context, pool *pgxpool.Pool, blobDir, version, claimedSha string, blob []byte) (string, error) {
	sum := sha256.Sum256(blob)
	sha := hex.EncodeToString(sum[:])
	if !strings.EqualFold(sha, claimedSha) {
		return "", ErrShaMismatch // nothing persisted
	}
	blobPath := sha + ".bin"

	// Reserve the row first (§4.4.1 step 3) — the version PK turns a duplicate into 23505 → the handler
	// maps it to 409, and no blob is (re)written over an in-use version (T6).
	if _, err := pool.Exec(ctx,
		`INSERT INTO firmware_versions (version, sha256, blob_path, size_bytes) VALUES ($1,$2,$3,$4)`,
		version, sha, blobPath, len(blob)); err != nil {
		return "", err
	}
	if err := writeBlobAtomic(blobDir, blobPath, blob); err != nil {
		return "", fmt.Errorf("blob write: %w", err)
	}
	return sha, nil
}

// writeBlobAtomic writes data to dir/name via a temp file + rename (atomic on the same filesystem), so
// a serve never observes a half-written blob.
func writeBlobAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-fw-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, filepath.Base(name)))
}

// SetChannelDefault points channels.default_version at version (FK-checked: an unknown version raises
// 23503 → the handler maps it to 422). found=false means the channel name does not exist (→ 404).
func SetChannelDefault(ctx context.Context, pool *pgxpool.Pool, channel, version string) (found bool, err error) {
	tag, err := pool.Exec(ctx, `UPDATE channels SET default_version = $2 WHERE name = $1`, channel, version)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SerialResolvable reports whether serial is a legal rollout target: the '*' fleet wildcard, or a
// registered device. rollout_targets.serial has no FK (the '*' precludes it, §4.4), so existence is
// checked here to 422 at the edge rather than inserting an orphan target.
func SerialResolvable(ctx context.Context, pool *pgxpool.Pool, serial string) (bool, error) {
	if serial == "*" {
		return true, nil
	}
	var ok bool
	err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices WHERE serial = $1)`, serial).Scan(&ok)
	return ok, err
}

// UpsertRollout creates or updates the rollout target for (serial, channel), forcing state='active'
// (re-activating a previously paused/done row). serial='*' is the channel fleet, else a per-serial pin.
// Unknown channel/version raise FK 23503 → the handler maps to 422. Returns the row id.
func UpsertRollout(ctx context.Context, pool *pgxpool.Pool, serial, channel, version string) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx,
		`INSERT INTO rollout_targets (serial, channel, version, state) VALUES ($1,$2,$3,'active')
		 ON CONFLICT (serial, channel) DO UPDATE SET version = $3, state = 'active', updated_at = now()
		 RETURNING id`,
		serial, channel, version).Scan(&id)
	return id, err
}

// SetRolloutState moves a target to active|paused|done (the value is validated at the edge). found=false
// means no row with that id (→ 404).
func SetRolloutState(ctx context.Context, pool *pgxpool.Pool, id int64, state string) (found bool, err error) {
	tag, err := pool.Exec(ctx, `UPDATE rollout_targets SET state = $2, updated_at = now() WHERE id = $1`, id, state)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteRollout removes a target (per-serial or fleet row) by id. found=false means no such row (→ 404).
func DeleteRollout(ctx context.Context, pool *pgxpool.Pool, id int64) (found bool, err error) {
	tag, err := pool.Exec(ctx, `DELETE FROM rollout_targets WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ---- admin-side list reads (auth-gated; the serve path reads via internal/rollout) ----

// FirmwareRow is a registered firmware version (no blob_path — that is an internal storage detail).
type FirmwareRow struct {
	Version   string    `json:"version"`
	Sha256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func ListFirmware(ctx context.Context, pool *pgxpool.Pool) ([]FirmwareRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT version, sha256, size_bytes, created_at FROM firmware_versions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FirmwareRow{}
	for rows.Next() {
		var f FirmwareRow
		if err := rows.Scan(&f.Version, &f.Sha256, &f.SizeBytes, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ChannelRow is a channel and its current default version (null = unset).
type ChannelRow struct {
	Name           string  `json:"name"`
	DefaultVersion *string `json:"default_version"`
}

func ListChannels(ctx context.Context, pool *pgxpool.Pool) ([]ChannelRow, error) {
	rows, err := pool.Query(ctx, `SELECT name, default_version FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChannelRow{}
	for rows.Next() {
		var c ChannelRow
		if err := rows.Scan(&c.Name, &c.DefaultVersion); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RolloutRow is a rollout target row for the management list.
type RolloutRow struct {
	ID        int64     `json:"id"`
	Serial    string    `json:"serial"`
	Channel   string    `json:"channel"`
	Version   string    `json:"version"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

func ListRollouts(ctx context.Context, pool *pgxpool.Pool) ([]RolloutRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, serial, channel, version, state, updated_at FROM rollout_targets ORDER BY channel, serial`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RolloutRow{}
	for rows.Next() {
		var r RolloutRow
		if err := rows.Scan(&r.ID, &r.Serial, &r.Channel, &r.Version, &r.State, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
