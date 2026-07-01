// Package bwry packs a raw 400x300 RGB frame into the PicPak panel's 2bpp BWRY
// buffer. It is the resize-FREE core of the reference prepare_image pipeline: a size
// gate (NO resize) -> flip_vertical (panel Y-mirror) -> nearest-palette quantize ->
// 2bpp MSB-first pack -> assert 30000 bytes. It is stdlib-only (no image/png, no
// decode path) and secret-free — the one correctness-critical mechanism of the FaaS
// engine, golden-vector locked (T8).
//
// Nearest quantize is EXACT squared-Euclidean argmin (lowest-index tie-break), NOT
// PIL's quantize(dither=NONE). Measured over the test fixture, PIL diverges from exact
// nearest on 0.59% of pixels and is STRICTLY FARTHER at every divergence (PIL_worse=714,
// PIL_better=0 of 120000 px) — its cube cache is a lossy approximation, so exact argmin
// is >= PIL everywhere. Anchoring the golden on exact nearest keeps the panel contract
// deterministic and independent of PIL's version, refining D24.3 while preserving its
// intent (a deterministic, resize-free nearest pack). Dithering is the worker's sharp;
// its already-palette pixels map 1:1 (distance 0) through this same nearest step.
package bwry

import (
	"fmt"
	"image"
	"image/color"
)

const (
	Width      = 400              // panel width  (picpak palette WIDTH)
	Height     = 300              // panel height (picpak palette HEIGHT)
	PackedSize = Width * Height / 4 // 30000 — 2bpp, 4 px/byte
	rawRGBLen  = Width * Height * 3 // 360000 — the fixed M4 raw frame size
)

// FromRGB wraps a raw, row-major, top-to-bottom RGB byte slice (exactly
// Width*Height*3 = 360000 bytes) as an opaque image.Image with NO decode. This is how
// the supervisor hands the worker's fixed-size raw output to Pack (D24.3/§4.4). An
// off-size slice is an error (the caller falls back), never a re-fit.
func FromRGB(pix []byte) (image.Image, error) {
	if len(pix) != rawRGBLen {
		return nil, fmt.Errorf("bwry: raw RGB length %d != %d", len(pix), rawRGBLen)
	}
	return &rgbImage{pix: pix}, nil
}

// Pack renders a 400x300 image into the 30000-byte BWRY panel buffer. Off-size input
// is a render error (the supervisor falls back, D24.6), never a silent re-fit — the
// worker already guarantees 400x300 (D24.3), so the size gate is a belt assertion.
func Pack(img image.Image) ([]byte, error) {
	b := img.Bounds()
	if b.Dx() != Width || b.Dy() != Height {
		return nil, fmt.Errorf("bwry: image %dx%d != %dx%d (no resize)", b.Dx(), b.Dy(), Width, Height)
	}
	out := pack2bpp(quantizeFlipped(img))
	if len(out) != PackedSize {
		return nil, fmt.Errorf("bwry: packed %d != %d", len(out), PackedSize)
	}
	return out, nil
}

// quantizeFlipped applies the panel Y-mirror (transpose FLIP_TOP_BOTTOM, on-device
// verified) and returns row-major BWRY codes: output row y reads source row Height-1-y.
// Omitting the flip ships an upside-down panel (T8 negative case).
func quantizeFlipped(img image.Image) []uint8 {
	b := img.Bounds()
	codes := make([]uint8, Width*Height)
	i := 0
	for y := 0; y < Height; y++ {
		sy := b.Min.Y + (Height - 1 - y)
		for x := 0; x < Width; x++ {
			r16, g16, b16, _ := img.At(b.Min.X+x, sy).RGBA()
			codes[i] = nearestCode(uint8(r16>>8), uint8(g16>>8), uint8(b16>>8))
			i++
		}
	}
	return codes
}

// pack2bpp packs BWRY codes (0..3) MSB-first, 4 px/byte:
// value |= (code & 3) << (6 - 2*i)  (the pack_pixel_codes invariant).
func pack2bpp(codes []uint8) []byte {
	out := make([]byte, (len(codes)+3)/4)
	for n := 0; n < len(codes); n += 4 {
		var v byte
		for i := 0; i < 4 && n+i < len(codes); i++ {
			v |= (codes[n+i] & 0x3) << (6 - 2*uint(i))
		}
		out[n/4] = v
	}
	return out
}

// rgbImage is a minimal opaque image.Image over a raw row-major RGB slice — no decode,
// no allocation beyond the wrapper. Alpha is always fully opaque.
type rgbImage struct{ pix []byte }

func (m *rgbImage) ColorModel() color.Model { return color.NRGBAModel }
func (m *rgbImage) Bounds() image.Rectangle { return image.Rect(0, 0, Width, Height) }
func (m *rgbImage) At(x, y int) color.Color {
	o := (y*Width + x) * 3
	return color.NRGBA{R: m.pix[o], G: m.pix[o+1], B: m.pix[o+2], A: 255}
}
