package imgstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Register the stdlib decoders image.DecodeConfig sniffs. png/jpeg cover the panel
	// pipeline with ZERO new module deps (masterplan §1); webp/avif would need
	// golang.org/x/image and is deferred to the A28 mime allowlist wave.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/jackc/pgx/v5"
)

// formatMIME maps an image.DecodeConfig format name to the mime the row stores. A format not
// listed here is rejected (ErrUnsupportedFormat) — the server sniffs the type, the client's
// claim is never trusted.
var formatMIME = map[string]string{
	"png":  "image/png",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
}

const imageCols = `id, operator_key_id, sha256, blob_path, mime, width, height, byte_size, created_at`

// sniff reads ONLY the image header (image.DecodeConfig — no full-image decode, no allocation
// of the pixel buffer) to derive the authoritative mime + dimensions. This is what makes the
// pixel-dimension cap cheap: a decompression bomb is caught from its declared width*height
// without ever decoding it.
func sniff(blob []byte) (mime string, w, h int, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(blob))
	if err != nil {
		return "", 0, 0, fmt.Errorf("%w: %v", ErrUnsupportedFormat, err)
	}
	m, ok := formatMIME[format]
	if !ok {
		return "", 0, 0, fmt.Errorf("%w: %s", ErrUnsupportedFormat, format)
	}
	return m, cfg.Width, cfg.Height, nil
}

// blobFSPath derives the on-disk path from the CHECKed sha256 column, never from the free
// blob_path column (§4.1 traversal defense: a divergent blob_path must not become a
// filepath.Join escape). filepath.Base is belt-and-suspenders on top of the DB hex CHECK.
func blobFSPath(blobDir, sha string) string {
	return filepath.Join(blobDir, filepath.Base(sha+".bin"))
}

// PutImage server-sniffs the mime + dimensions, enforces the pixel-dimension cap (decompression
// bomb, §5) BEFORE touching the DB or the blob volume, then content-addressed-upserts the row:
// a second upload of identical bytes returns the EXISTING row (dedup, no second blob write,
// E27.4 return-existing). A newly reserved row is written to the DB first, then the blob is
// written atomically (temp+rename, rolloutadmin pattern) — a crash in between orphans a row over
// a missing blob (reconcile sweep, W6), never a blob under a sha the bytes never had.
func PutImage(ctx context.Context, q Querier, blobDir string, operatorKeyID *int64, blob []byte) (Image, error) {
	mime, w, h, err := sniff(blob)
	if err != nil {
		return Image{}, err
	}
	// Decompression-bomb guard: reject oversized decoded dimensions BEFORE any persistence.
	if int64(w)*int64(h) > MaxImagePixels {
		return Image{}, ErrImageTooLarge
	}

	sum := sha256.Sum256(blob)
	sha := hex.EncodeToString(sum[:])
	blobPath := sha + ".bin"

	// Reserve the row. ON CONFLICT (sha256) DO NOTHING → a duplicate returns no row; we then
	// read the existing one (return-existing dedup) and skip the blob write (already present).
	img, err := scanImage(q.QueryRow(ctx, `
		INSERT INTO image (operator_key_id, sha256, blob_path, mime, width, height, byte_size)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (sha256) DO NOTHING
		RETURNING `+imageCols,
		operatorKeyID, sha, blobPath, mime, w, h, len(blob)))
	if errors.Is(err, ErrNotFound) {
		// Dedup hit: existing row. The blob is NORMALLY on disk already — but a crash
		// between a previous row insert and its blob write leaves a row without a blob
		// forever (row-before-blob order). Heal that window here: we hold the exact
		// bytes, so re-write the blob when it is missing (one cheap stat on this path).
		if _, statErr := os.Stat(blobFSPath(blobDir, sha)); os.IsNotExist(statErr) {
			if healErr := writeBlobAtomic(blobDir, blobPath, blob); healErr != nil {
				return Image{}, fmt.Errorf("blob heal: %w", healErr)
			}
		}
		return GetImageBySha(ctx, q, sha)
	}
	if err != nil {
		return Image{}, err
	}
	// New row reserved → write the blob last (row-before-blob crash order).
	if err := writeBlobAtomic(blobDir, blobPath, blob); err != nil {
		return Image{}, fmt.Errorf("blob write: %w", err)
	}
	return img, nil
}

// DeleteImage removes an image row then its blob (closing the fwblobs no-delete gap). A FK
// RESTRICT from playlist_item (W2+) surfaces as 23503 → ErrImageInUse (→ 409): an in-use image
// cannot be deleted. The blob is removed AFTER the row (idempotent os.Remove tolerates a missing
// file); the FS path comes from the CHECKed sha256, not the free blob_path (traversal defense).
func DeleteImage(ctx context.Context, q Querier, blobDir string, id int64) error {
	var sha string
	err := q.QueryRow(ctx, `DELETE FROM image WHERE id = $1 RETURNING sha256`, id).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrImageInUse
		}
		return err
	}
	if err := os.Remove(blobFSPath(blobDir, sha)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeleteImageManagedAware is the "Aufs Panel"-aware image delete (design/33 §4.5b cleanup c): before
// giving up with a 409, it clears any MANAGED single-image playlists that reference the image — those
// exist only to hold that one image for one panel, so an image delete legitimately unbinds the panel
// and reaps the playlist rather than pinning the image undeletable. It stays a 409 (ErrImageInUse) only
// when a NON-managed (operator-authored) playlist still references the image: a real playlist's content
// is operator-owned and never silently rehomed by an image delete. Runs in one tx:
//
//   - if any operator playlist references the image → ErrImageInUse (rolled back, no side effects);
//   - else drop the referencing managed playlists (cascading their lone item + device binding) after
//     clearing their FK-less rotation cursors, then delete the now-unreferenced image row + blob.
func DeleteImageManagedAware(ctx context.Context, pool Pool, blobDir string, id int64) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	var inReal bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM playlist_item pi JOIN playlist p ON p.id = pi.playlist_id
			WHERE pi.image_id = $1 AND p.managed_serial IS NULL)`, id).Scan(&inReal); err != nil {
		return err
	}
	if inReal {
		return ErrImageInUse
	}
	// Only managed playlists (or nothing) reference the image. Clear their FK-less cursors first (a
	// playlist delete cannot cascade playlist_cursor — it has no FK), then drop the managed playlists.
	if _, err := tx.Exec(ctx, `
		DELETE FROM playlist_cursor WHERE serial IN (
			SELECT p.managed_serial FROM playlist p
			JOIN playlist_item pi ON pi.playlist_id = p.id
			WHERE pi.image_id = $1 AND p.managed_serial IS NOT NULL)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM playlist WHERE managed_serial IS NOT NULL AND id IN (
			SELECT playlist_id FROM playlist_item WHERE image_id = $1)`, id); err != nil {
		return err
	}
	var sha string
	err = tx.QueryRow(ctx, `DELETE FROM image WHERE id = $1 RETURNING sha256`, id).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrImageInUse // a concurrent AddItem to a real playlist beat us — stays a 409
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Blob after the committed row (idempotent os.Remove tolerates a missing file); the FS path comes
	// from the CHECKed sha256, not the free blob_path (traversal defense).
	if err := os.Remove(blobFSPath(blobDir, sha)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LoadBlob reads an image's bytes (supervisor render read, :ro mount) plus its metadata. The FS
// path is derived from the CHECKed sha256, not blob_path (traversal defense, §4.1).
func LoadBlob(ctx context.Context, q Querier, blobDir string, id int64) ([]byte, Image, error) {
	img, err := GetImage(ctx, q, id)
	if err != nil {
		return nil, Image{}, err
	}
	b, err := os.ReadFile(blobFSPath(blobDir, img.Sha256))
	if err != nil {
		return nil, Image{}, err
	}
	return b, img, nil
}

// GetImage returns an image row by id or ErrNotFound.
func GetImage(ctx context.Context, q Querier, id int64) (Image, error) {
	return scanImage(q.QueryRow(ctx, `SELECT `+imageCols+` FROM image WHERE id = $1`, id))
}

// GetImageBySha returns an image row by content address or ErrNotFound (dedup lookup).
func GetImageBySha(ctx context.Context, q Querier, sha string) (Image, error) {
	return scanImage(q.QueryRow(ctx, `SELECT `+imageCols+` FROM image WHERE sha256 = $1`, sha))
}

// ListImages returns a keyset page ordered by id (WHERE id > cursor). Pass cursor=0 for the
// first page; the last returned id is the next cursor. Keyset, never OFFSET (§6 pagination).
func ListImages(ctx context.Context, q Querier, limit int, cursor int64) ([]Image, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT `+imageCols+` FROM image WHERE id > $1 ORDER BY id LIMIT $2`, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Image{}
	for rows.Next() {
		img, err := scanImageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// SweepOrphanBlobs reconciles the imgblobs volume against the image table (maintenance, W6 / §6). It
// closes the two crash windows PutImage/DeleteImage document: a blob written for a row whose insert
// never committed (or a delete that removed the row before its blob) leaves a content-addressed file
// with NO row → an orphan the volume would otherwise carry forever (blob-cost monotonic growth at
// fleet scale, §6). Direction 1 (removed): every `<sha>.bin` (and every leftover atomic-write temp
// file) with no image row AND older than grace is deleted. Direction 2 (missing): a row whose blob is
// absent is only COUNTED for the caller to log — the bytes are gone from here, and PutImage self-heals
// that window on the next identical upload; the sweep never fabricates bytes.
//
// grace protects the write→commit race: PutImage writes the blob while the caller tx may still be
// uncommitted, so a blob younger than grace can belong to an as-yet-invisible row — deleting it would
// destroy a live upload. This is a refcount/existence reconcile, NOT a created_at TTL (a stable, hot
// image keeps its row and always survives regardless of blob age). blobDir absent ⇒ a no-op (0,0,nil):
// nothing has been written yet. q must see committed rows (run on the pool, not inside a writer tx).
func SweepOrphanBlobs(ctx context.Context, q Querier, blobDir string, grace time.Duration, now time.Time) (removed, missing int, err error) {
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	// The authoritative content-address set (committed rows only).
	known := map[string]struct{}{}
	rows, err := q.Query(ctx, `SELECT sha256 FROM image`)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, 0, err
		}
		known[sha] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	onDisk := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Leftover atomic-write temp files (writeBlobAtomic pattern ".tmp-img-*") from a crashed
		// PutImage are orphans by definition — never content-addressed, never referenced. Grace still
		// shields one that a live write is mid-rename.
		if strings.HasPrefix(name, ".tmp-img-") {
			if info, ierr := e.Info(); ierr == nil && now.Sub(info.ModTime()) >= grace {
				if rerr := os.Remove(filepath.Join(blobDir, name)); rerr != nil && !os.IsNotExist(rerr) {
					return removed, missing, rerr
				}
				removed++
			}
			continue
		}
		sha, ok := strings.CutSuffix(name, ".bin")
		if !ok {
			continue // foreign file — never touched
		}
		onDisk[sha] = struct{}{}
		if _, hasRow := known[sha]; hasRow {
			continue // referenced blob survives at ANY age (the anti-TTL invariant)
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if now.Sub(info.ModTime()) < grace {
			continue // young orphan — protect the write→commit window
		}
		if rerr := os.Remove(filepath.Join(blobDir, name)); rerr != nil && !os.IsNotExist(rerr) {
			return removed, missing, rerr
		}
		removed++
	}

	// Direction 2: a committed row whose blob is missing — counted, not acted on (heal is PutImage's).
	for sha := range known {
		if _, ok := onDisk[sha]; !ok {
			missing++
		}
	}
	return removed, missing, nil
}

// --- helpers ---

// scanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query loop).
type scanner interface {
	Scan(dest ...any) error
}

func scanImageRow(s scanner) (Image, error) {
	var img Image
	err := s.Scan(&img.ID, &img.OperatorKeyID, &img.Sha256, &img.BlobPath,
		&img.Mime, &img.Width, &img.Height, &img.ByteSize, &img.CreatedAt)
	return img, err
}

func scanImage(row pgx.Row) (Image, error) {
	img, err := scanImageRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	return img, nil
}

// writeBlobAtomic writes data to dir/name via a temp file + rename (atomic on the same
// filesystem), so a reader never observes a half-written blob (rolloutadmin.writeBlobAtomic
// pattern). name is content-addressed (<sha>.bin); filepath.Base guards the join.
func writeBlobAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-img-*")
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
