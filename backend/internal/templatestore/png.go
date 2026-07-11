package templatestore

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"

	"github.com/open-picpak/backend/internal/bwry"
)

// previewPalette mirrors the logical BWRY palette (internal/bwry/quantize.go: 0=K 1=W 2=Y 3=R). A
// packed frame carries palette CODES, so the preview PNG maps each code back to its logical RGB. Kept
// in step with bwry's `palette` and the SPA decoder (bwrydecode.ts PALETTE) — a swapped order is a
// silent colour bug (the golden PNG probe pins it).
var previewPalette = color.Palette{
	color.RGBA{0, 0, 0, 255},       // 0 = K (black)
	color.RGBA{255, 255, 255, 255}, // 1 = W (white)
	color.RGBA{255, 255, 0, 255},   // 2 = Y (yellow)
	color.RGBA{255, 0, 0, 255},     // 3 = R (red)
}

// PackedToPNG decodes a 30000-byte 2bpp-packed BWRY frame into a 4-colour paletted PNG (stdlib
// image/png, NO new dependency — §4.5a). The packed buffer is already display-oriented (bwry.Pack
// applies the panel Y-mirror during pack), so the decode is a straight row-major unpack with no
// re-flip — exactly the inverse of pack2bpp / bwrydecode.ts. An off-size frame is an error (the
// caller falls back to the error frame), never a partial image.
func PackedToPNG(packed []byte) ([]byte, error) {
	if len(packed) != bwry.PackedSize {
		return nil, fmt.Errorf("templatestore: packed frame %d bytes != %d", len(packed), bwry.PackedSize)
	}
	img := image.NewPaletted(image.Rect(0, 0, bwry.Width, bwry.Height), previewPalette)
	for px := 0; px < bwry.Width*bwry.Height; px++ {
		// 2bpp MSB-first unpack: code = (byte >> (6 - 2*(px&3))) & 3, 4 px/byte (inverse of pack2bpp).
		code := (packed[px>>2] >> (6 - 2*uint(px&3))) & 0x3
		img.Pix[px] = code // Paletted.Pix is one palette index per pixel, row-major
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("templatestore: png encode: %w", err)
	}
	return buf.Bytes(), nil
}
