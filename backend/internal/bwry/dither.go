package bwry

import (
	"fmt"
	"image"
	"math"
)

// Dither selects the palette-conversion algorithm used by PackWithDither.
//
// The mechanism (dither algorithm) is code; the selection (which mode a function
// runs) is data carried in trigger_config (A31.2). This type is the single source
// of truth for both the render path and the admin-side validation (A31.3).
type Dither int

const (
	// DitherNone is the historical exact, UNweighted squared-Euclidean nearest
	// quantize (quantize.go nearestCode) with NO error diffusion. PackWithDither
	// with this mode is byte-identical to Pack — the golden_none fleet contract
	// must never move (A31-E2, board-decided: no fleet regression).
	DitherNone Dither = iota
	// DitherAtkinson is BT.601-luma-weighted nearest + Atkinson error diffusion,
	// reference-faithful to the vendor pipeline (documentation/image-pipeline.md
	// §3/§4/§7, lines 116-155): float32 work buffers, float64 arithmetic, error
	// accumulated UNclamped, upright dither then vertical flip.
	DitherAtkinson
	// DitherFloydSteinberg, DitherOrdered: reserved enum values (catalog.ts:57
	// promises the vocabulary) — NOT implemented in A31.1; ParseDither rejects
	// their strings fail-closed until built.
)

// ParseDither maps a trigger_config dither string to a Dither mode. Only the
// implemented modes ("none"/"atkinson") parse; every other string — including
// the empty string, typos ("atknson"), and reserved-but-unbuilt modes
// ("floyd-steinberg"/"ordered") — fails closed (ok=false). The caller rejects
// (422 invalid_dither, A31.3) rather than silently degrading to none.
func ParseDither(s string) (Dither, bool) {
	switch s {
	case "none":
		return DitherNone, true
	case "atkinson":
		return DitherAtkinson, true
	default:
		return 0, false
	}
}

// paletteF is the logical BWRY palette as float64 for the reference-faithful
// dither arithmetic (same anchors as `palette` in quantize.go: 0=K 1=W 2=Y 3=R).
var paletteF = [4][3]float64{
	{0, 0, 0},       // 0 = K (black)
	{255, 255, 255}, // 1 = W (white)
	{255, 255, 0},   // 2 = Y (yellow)
	{255, 0, 0},     // 3 = R (red)
}

// lumaW are the ITU-R BT.601 luma weights (image-pipeline.md:121). These are
// used ONLY in the dither path's distance metric — the DitherNone path stays on
// the unweighted metric (quantize.go) so its golden is unaffected (A31-E2).
var lumaW = [3]float64{0.299, 0.587, 0.114}

// PackWithDither renders a 400x300 image into the 30000-byte BWRY panel buffer
// using the given dither mode. Off-size input is a render error, never a re-fit
// (D24.3). DitherNone reproduces Pack byte-for-byte; DitherAtkinson runs the
// reference error-diffusion pipeline (upright dither -> vertical flip -> 2bpp
// MSB-first pack).
func PackWithDither(img image.Image, d Dither) ([]byte, error) {
	b := img.Bounds()
	if b.Dx() != Width || b.Dy() != Height {
		return nil, fmt.Errorf("bwry: image %dx%d != %dx%d (no resize)", b.Dx(), b.Dy(), Width, Height)
	}
	var codes []uint8
	switch d {
	case DitherNone:
		codes = quantizeFlipped(img)
	case DitherAtkinson:
		codes = flipCodes(quantizeDitherUpright(img))
	default:
		return nil, fmt.Errorf("bwry: unknown dither mode %d", int(d))
	}
	out := pack2bpp(codes)
	if len(out) != PackedSize {
		return nil, fmt.Errorf("bwry: packed %d != %d", len(out), PackedSize)
	}
	return out, nil
}

// nearestCodeBT601 returns the BWRY code minimising BT.601-luma-weighted squared
// RGB distance to the logical palette (image-pipeline.md:135-137). Tie-break is
// "first minimum" (strict d < bd), matching the reference's argmin. Operands are
// float64; PAL and the inputs come from float32 storage but promote exactly.
func nearestCodeBT601(r, g, b float64) int {
	best := 0
	bd := math.Inf(1)
	for k := 0; k < 4; k++ {
		dr := r - paletteF[k][0]
		dg := g - paletteF[k][1]
		db := b - paletteF[k][2]
		d := lumaW[0]*dr*dr + lumaW[1]*dg*dg + lumaW[2]*db*db
		if d < bd {
			bd = d
			best = k
		}
	}
	return best
}

// quantizeDitherUpright runs BT.601 nearest + Atkinson error diffusion on the
// UPRIGHT image and returns row-major codes (NOT flipped — the flip is a separate
// step, since dither must run over all upright rows before the Y-mirror, unlike
// quantizeFlipped which fuses quantize+flip and is therefore not reusable here).
//
// The arithmetic domain deliberately matches the JS reference (image-pipeline.md
// :123-144): work buffers are float32 (JS Float32Array), all reads promote to
// float64, distance and error math are float64 (JS Number), and each neighbour
// store truncates back to float32 (r[j] = float32(float64(r[j]) + eR)). The error
// accumulates UNclamped — the unclamped negative red error is what keeps green
// conservative and dark (image-pipeline.md:83-86). Go performs no excess
// precision and no implicit FMA across these separate statements, so the result
// is bit-deterministic AND reproduces the reference bytes.
func quantizeDitherUpright(img image.Image) []uint8 {
	bnd := img.Bounds()
	r := make([]float32, Width*Height)
	g := make([]float32, Width*Height)
	bl := make([]float32, Width*Height)
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			r16, g16, b16, _ := img.At(bnd.Min.X+x, bnd.Min.Y+y).RGBA()
			i := y*Width + x
			r[i] = float32(uint8(r16 >> 8))
			g[i] = float32(uint8(g16 >> 8))
			bl[i] = float32(uint8(b16 >> 8))
		}
	}
	code := make([]uint8, Width*Height)
	// Atkinson kernel: 6 neighbours, 1/8 each (image-pipeline.md:80/128).
	kernel := [6][2]int{{1, 0}, {2, 0}, {-1, 1}, {0, 1}, {1, 1}, {0, 2}}
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			i := y*Width + x
			R, G, B := float64(r[i]), float64(g[i]), float64(bl[i])
			best := nearestCodeBT601(R, G, B)
			code[i] = uint8(best)
			eR := (R - paletteF[best][0]) / 8
			eG := (G - paletteF[best][1]) / 8
			eB := (B - paletteF[best][2]) / 8
			for _, k := range kernel {
				nx, ny := x+k[0], y+k[1]
				if nx >= 0 && nx < Width && ny >= 0 && ny < Height {
					j := ny*Width + nx
					r[j] = float32(float64(r[j]) + eR)
					g[j] = float32(float64(g[j]) + eG)
					bl[j] = float32(float64(bl[j]) + eB)
				}
			}
		}
	}
	return code
}

// flipCodes applies the panel Y-mirror to an upright code plane: output row y
// reads source row Height-1-y (image-pipeline.md:93-96). This is the same index
// math as quantizeFlipped but on pre-computed codes, because the dither pass must
// complete upright before the flip.
func flipCodes(upright []uint8) []uint8 {
	out := make([]uint8, len(upright))
	for y := 0; y < Height; y++ {
		src := (Height - 1 - y) * Width
		copy(out[y*Width:(y+1)*Width], upright[src:src+Width])
	}
	return out
}
