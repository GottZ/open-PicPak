package imgstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB property tests — skipped unless TEST_DATABASE_URL is set (run in the e2e gate against an
// ephemeral postgres with migrations 0001..0011 applied). Mirrors faasstore/store_test.go.
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
	if _, err := pool.Exec(context.Background(), `TRUNCATE image RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// tinyPNG is a real, decodable 2×2 RGBA PNG (a valid non-bomb input).
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode tiny png: %v", err)
	}
	return buf.Bytes()
}

// bombPNG builds a PNG that is a few dozen bytes on the wire but declares w×h pixels in its
// IHDR. image.DecodeConfig reads only the header (correct IHDR CRC required) → it returns the
// huge dimensions without decoding, which is exactly the decompression-bomb the pixel cap must
// catch. No IDAT/IEND is needed: DecodeConfig returns right after a non-paletted IHDR.
func bombPNG(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) // signature
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	_ = binary.Write(&ihdr, binary.BigEndian, w)
	_ = binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // bit depth 8, color type 2 (RGB, non-paletted), no compression/filter/interlace
	_ = binary.Write(&b, binary.BigEndian, uint32(ihdr.Len()-4))  // chunk length (excludes the 4-byte type)
	b.Write(ihdr.Bytes())                                          // "IHDR" + data
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes())) // CRC over type+data
	return b.Bytes()
}

// TestSniffRejectsNonImage is DB-independent (runs in -short): non-image bytes never reach the
// store.
func TestSniffRejectsNonImage(t *testing.T) {
	if _, _, _, err := sniff([]byte("this is not an image")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("sniff garbage: want ErrUnsupportedFormat, got %v", err)
	}
}

// TestPutImageRejectsDecompressionBomb is the W1 negative probe (§7 / masterplan Phase 1 gate):
// an image whose declared dimensions exceed MaxImagePixels MUST be rejected with ErrImageTooLarge
// BEFORE any row or blob is persisted. Without the cap the bomb is accepted (row + blob written) —
// that is the red state this probe fails on.
func TestPutImageRejectsDecompressionBomb(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	// 8000×8000 = 64 MPix > 24 MPix cap; DecodeConfig accepts these dims (no internal guard fires).
	bomb := bombPNG(8000, 8000)
	// sanity: the header really does decode to the oversized dimensions (bomb is well-formed).
	if _, w, h, err := sniff(bomb); err != nil || int64(w)*int64(h) <= MaxImagePixels {
		t.Fatalf("bomb fixture invalid: w=%d h=%d err=%v (want decodable, over cap)", w, h, err)
	}

	_, err := PutImage(ctx, pool, blobDir, nil, bomb)
	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("bomb accepted: want ErrImageTooLarge, got %v", err)
	}
	// No persistence side effects.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bomb left a row: count=%d err=%v", n, err)
	}
	files, _ := os.ReadDir(blobDir)
	if len(files) != 0 {
		t.Fatalf("bomb wrote a blob: %v", files)
	}
}

// TestPutImageDedupAndBlob: a valid image reserves a row + writes its blob; re-uploading the same
// bytes returns the existing row (E27.4 return-existing), no duplicate, no second blob.
func TestPutImageDedupAndBlob(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()
	blob := tinyPNG(t)

	kid := int64(0) // no operator_keys row seeded here → attribute as NULL
	var opKey *int64
	if kid != 0 {
		opKey = &kid
	}

	img, err := PutImage(ctx, pool, blobDir, opKey, blob)
	if err != nil {
		t.Fatalf("PutImage: %v", err)
	}
	if img.Mime != "image/png" || img.Width != 2 || img.Height != 2 || img.ByteSize != int64(len(blob)) {
		t.Fatalf("metadata: %+v", img)
	}
	if _, err := os.Stat(filepath.Join(blobDir, img.Sha256+".bin")); err != nil {
		t.Fatalf("blob not written: %v", err)
	}

	// Re-upload identical bytes → same id, no duplicate row, still one blob.
	img2, err := PutImage(ctx, pool, blobDir, opKey, blob)
	if err != nil || img2.ID != img.ID {
		t.Fatalf("dedup: id=%d (want %d) err=%v", img2.ID, img.ID, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM image`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dedup left duplicates: count=%d err=%v", n, err)
	}
	files, _ := os.ReadDir(blobDir)
	if len(files) != 1 {
		t.Fatalf("dedup wrote a second blob: %v", files)
	}

	// Round-trip the bytes back out.
	got, meta, err := LoadBlob(ctx, pool, blobDir, img.ID)
	if err != nil || !bytes.Equal(got, blob) || meta.Sha256 != img.Sha256 {
		t.Fatalf("LoadBlob: len=%d err=%v", len(got), err)
	}
}

// TestDeleteImage: delete removes the row + its blob; deleting a missing id is ErrNotFound.
func TestDeleteImage(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()

	img, err := PutImage(ctx, pool, blobDir, nil, tinyPNG(t))
	if err != nil {
		t.Fatalf("PutImage: %v", err)
	}
	blobFile := filepath.Join(blobDir, img.Sha256+".bin")

	if err := DeleteImage(ctx, pool, blobDir, img.ID); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if _, err := GetImage(ctx, pool, img.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row survived delete: %v", err)
	}
	if _, err := os.Stat(blobFile); !os.IsNotExist(err) {
		t.Fatalf("blob survived delete: %v", err)
	}
	if err := DeleteImage(ctx, pool, blobDir, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: want ErrNotFound, got %v", err)
	}
}

// TestPutImageHealsMissingBlob proves the crash-window heal (review fix on W1): a crash between
// row insert and blob write leaves a row without a blob; the next PutImage of the SAME bytes is a
// dedup hit and must RE-WRITE the missing blob instead of assuming it exists.
func TestPutImageHealsMissingBlob(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	blobDir := t.TempDir()
	blob := tinyPNG(t)

	img, err := PutImage(ctx, pool, blobDir, nil, blob)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	blobFile := filepath.Join(blobDir, img.Sha256+".bin")
	// Simulate the crash window: row exists, blob gone.
	if err := os.Remove(blobFile); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	img2, err := PutImage(ctx, pool, blobDir, nil, blob)
	if err != nil {
		t.Fatalf("dedup put: %v", err)
	}
	if img2.ID != img.ID {
		t.Fatalf("dedup id mismatch: %d != %d", img2.ID, img.ID)
	}
	if _, err := os.Stat(blobFile); err != nil {
		t.Fatalf("blob not healed on dedup hit: %v", err)
	}
	got, _, err := LoadBlob(ctx, pool, blobDir, img.ID)
	if err != nil || len(got) != len(blob) {
		t.Fatalf("healed blob unreadable: err=%v len=%d want %d", err, len(got), len(blob))
	}
}
