package bwry

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"
)

func loadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

// T8 — BWRY pack golden vector (resize-free core). Pack(fixture) must equal the
// committed golden byte-for-byte. The fixture is a NON-uniform 400x300 frame (all four
// codes present, y-asymmetric) so the gate exercises flip + per-pixel nearest + 2bpp,
// not a flat-colour degenerate. The golden is anchored on exact-argmin nearest (see the
// package doc) so it is PIL-version-independent.
func TestPackGolden(t *testing.T) {
	fixture := loadTestdata(t, "fixture.bin")
	golden := loadTestdata(t, "golden_none.bin")
	if len(golden) != PackedSize {
		t.Fatalf("golden len %d != %d", len(golden), PackedSize)
	}
	img, err := FromRGB(fixture)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}
	got, err := Pack(img)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if !bytes.Equal(got, golden) {
		for i := range got {
			if got[i] != golden[i] {
				t.Fatalf("golden mismatch at byte %d: got %#02x want %#02x", i, got[i], golden[i])
			}
		}
	}
}

// T8 negative probes — each corruption MUST diverge from the golden, proving the
// property is load-bearing (a "looks-right" pack that is one of these silently ships a
// corrupt panel).
func TestPackNegativeProbes(t *testing.T) {
	fixture := loadTestdata(t, "fixture.bin")
	golden := loadTestdata(t, "golden_none.bin")
	img, err := FromRGB(fixture)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}

	// (a) dropped Y-flip: feeding a pre-flipped frame makes Pack flip twice -> original
	// orientation -> different bytes. Load-bearing: omitting the flip ships upside-down.
	flipped := make([]byte, len(fixture))
	for y := 0; y < Height; y++ {
		copy(flipped[y*Width*3:(y+1)*Width*3], fixture[(Height-1-y)*Width*3:(Height-y)*Width*3])
	}
	fimg, err := FromRGB(flipped)
	if err != nil {
		t.Fatalf("FromRGB(flipped): %v", err)
	}
	if got, _ := Pack(fimg); bytes.Equal(got, golden) {
		t.Error("dropped-flip probe: double-flipped pack unexpectedly equals golden")
	}

	codes := quantizeFlipped(img)

	// (b) LSB-first pack instead of MSB-first -> mismatch.
	lsb := make([]byte, PackedSize)
	for n := 0; n < len(codes); n += 4 {
		var v byte
		for i := 0; i < 4 && n+i < len(codes); i++ {
			v |= (codes[n+i] & 0x3) << (2 * uint(i)) // wrong: LSB-first
		}
		lsb[n/4] = v
	}
	if bytes.Equal(lsb, golden) {
		t.Error("LSB-first probe: unexpectedly equals golden")
	}

	// (c) swapped palette order (K<->W) -> different codes -> mismatch.
	swap := make([]uint8, len(codes))
	for i, c := range codes {
		switch c {
		case 0:
			swap[i] = 1
		case 1:
			swap[i] = 0
		default:
			swap[i] = c
		}
	}
	swapped := pack2bpp(swap)
	if bytes.Equal(swapped, golden) {
		t.Error("swapped-palette probe: unexpectedly equals golden")
	}
}

// Size gate: off-size input is a render error, never a silent re-fit (D24.3).
func TestSizeGate(t *testing.T) {
	if _, err := FromRGB(make([]byte, rawRGBLen-3)); err == nil {
		t.Error("FromRGB accepted a short raw slice")
	}
	if _, err := FromRGB(make([]byte, rawRGBLen+3)); err == nil {
		t.Error("FromRGB accepted an over-long raw slice")
	}
	// A validly-typed but wrong-size image must be rejected by Pack.
	small := image.NewNRGBA(image.Rect(0, 0, Width, Height-1))
	if _, err := Pack(small); err == nil {
		t.Error("Pack accepted a 400x299 image")
	}
}

// nearestCode: exact palette colours map to their own code (the dithered-frame 1:1
// property, D24.3) and boundary pixels resolve to the strictly-nearest code.
func TestNearestCode(t *testing.T) {
	for c := 0; c < 4; c++ {
		r, g, b := uint8(palette[c][0]), uint8(palette[c][1]), uint8(palette[c][2])
		if got := nearestCode(r, g, b); int(got) != c {
			t.Errorf("nearestCode(exact palette %d) = %d, want %d", c, got, c)
		}
	}
	cases := []struct {
		r, g, b uint8
		want    uint8
	}{
		{10, 10, 10, 0},     // near black -> K
		{250, 250, 250, 1},  // near white -> W
		{250, 250, 10, 2},   // near yellow -> Y
		{250, 10, 10, 3},    // near red -> R
		{120, 0, 0, 0},      // between K(0,0,0) and R(255,0,0), closer to K
		{140, 0, 0, 3},      // between K and R, closer to R
	}
	for _, tc := range cases {
		if got := nearestCode(tc.r, tc.g, tc.b); got != tc.want {
			t.Errorf("nearestCode(%d,%d,%d) = %d, want %d", tc.r, tc.g, tc.b, got, tc.want)
		}
	}
}

// A frame of exact palette colours packs to exactly those codes (distance 0) — the
// worker's dithered (already-palette) output maps 1:1 through the same nearest step.
func TestPalettePassthrough(t *testing.T) {
	pix := make([]byte, rawRGBLen)
	for p := 0; p < Width*Height; p++ {
		c := p % 4
		pix[p*3], pix[p*3+1], pix[p*3+2] = uint8(palette[c][0]), uint8(palette[c][1]), uint8(palette[c][2])
	}
	img, err := FromRGB(pix)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}
	codes := quantizeFlipped(img)
	// Every source pixel is an exact palette colour, so its code is its own index.
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			src := ((Height-1-y)*Width + x) % 4 // quantizeFlipped mirrors rows
			if got := codes[y*Width+x]; int(got) != src {
				t.Fatalf("passthrough mismatch at (%d,%d): got %d want %d", x, y, got, src)
			}
		}
	}
}
