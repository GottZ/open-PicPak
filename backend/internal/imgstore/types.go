// Package imgstore is the content-addressed source-image store (migration 0011, A27 W1):
// operator-attributed image rows keyed by sha256, with a dedup upsert on ingest and a
// delete path that closes the fwblobs "no delete" gap. Multi-MB source blobs live in the
// content-addressed imgblobs volume (sha256||'.bin'); the DB row carries only metadata
// (mime/dimensions/size). PutImage enforces a pixel-dimension cap BEFORE any blob is
// written, so a small-compressed / huge-decoded image (decompression bomb, §5) is rejected
// before it can OOM a downstream decoder.
//
// All access goes through a Querier so a call can run on the pool or inside a tx (e.g. the
// device-delete cascade / maintenance sweeps). The FS path is always derived from the CHECKed
// sha256 column (filepath.Base), never from the free blob_path column — a divergent blob_path
// (bug/migration) must not become a filepath.Join traversal.
package imgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxImagePixels is the hard decoded-pixel ceiling PutImage enforces (decompression-bomb
// guard, §5). ~24 MPix ≈ 40× the 400×300 panel target — a byte cap alone cannot catch a
// few-hundred-KB PNG that decodes to gigabytes, so the cap is on width*height from the
// header (image.DecodeConfig), not on the compressed byte size.
const MaxImagePixels int64 = 24_000_000

var (
	// ErrNotFound is returned when an image row does not exist.
	ErrNotFound = errors.New("imgstore: not found")
	// ErrImageTooLarge is returned when the decoded dimensions exceed MaxImagePixels
	// (decompression bomb). Nothing is persisted — no row, no blob. Handler maps it to 413.
	ErrImageTooLarge = errors.New("imgstore: image dimensions exceed cap")
	// ErrUnsupportedFormat is returned when the bytes are not a decodable image of a
	// supported format (png/jpeg). Handler maps it to 415.
	ErrUnsupportedFormat = errors.New("imgstore: unsupported image format")
	// ErrImageInUse is returned when DeleteImage hits a FK RESTRICT (23503) — an image
	// referenced by a playlist_item (W2+) cannot be deleted. Handler maps it to 409.
	ErrImageInUse = errors.New("imgstore: image is in use")
)

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Pool is a Querier that can open a transaction — DeleteImageManagedAware needs it to run its
// managed-playlist cleanup + image delete in one tx. *pgxpool.Pool satisfies it.
type Pool interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Image is a stored source image (metadata; the bytes live in the imgblobs volume).
type Image struct {
	ID            int64     `json:"id"`
	OperatorKeyID *int64    `json:"operator_key_id,omitempty"`
	Sha256        string    `json:"sha256"`
	BlobPath      string    `json:"-"` // internal storage detail; never surfaced in an API read
	Mime          string    `json:"mime"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	ByteSize      int64     `json:"byte_size"`
	CreatedAt     time.Time `json:"created_at"`
}

// IsForeignKeyViolation reports whether err is a Postgres FK violation (23503) — DeleteImage
// maps an in-use image (referenced by a playlist_item, W2+) to ErrImageInUse → 409.
func IsForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}
