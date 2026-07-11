package templatestore

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/open-picpak/backend/internal/bwry"
)

// packCode builds a 30000-byte packed frame with every pixel set to one BWRY code (MSB-first 4px/byte).
func packCodeFrame(code byte) []byte {
	b := (code & 3) | (code&3)<<2 | (code&3)<<4 | (code&3)<<6
	f := make([]byte, bwry.PackedSize)
	for i := range f {
		f[i] = b
	}
	return f
}

// TestPackedToPNG_PaletteAndOrientation pins the 2bpp→PNG decode: the 4-colour palette order
// (0=K 1=W 2=Y 3=R) and the straight row-major (already display-oriented) unpack. Red: a swapped
// palette entry or a re-flip changes the decoded corner colours and fails these asserts.
func TestPackedToPNG_PaletteAndOrientation(t *testing.T) {
	want := map[byte][3]uint8{
		0: {0, 0, 0}, 1: {255, 255, 255}, 2: {255, 255, 0}, 3: {255, 0, 0},
	}
	for code, rgb := range want {
		raw, err := PackedToPNG(packCodeFrame(code))
		if err != nil {
			t.Fatalf("code %d: encode: %v", code, err)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("code %d: decode: %v", code, err)
		}
		if b := img.Bounds(); b.Dx() != bwry.Width || b.Dy() != bwry.Height {
			t.Fatalf("code %d: bounds %v, want %dx%d", code, b, bwry.Width, bwry.Height)
		}
		r, g, bl, _ := img.At(0, 0).RGBA()
		if uint8(r>>8) != rgb[0] || uint8(g>>8) != rgb[1] || uint8(bl>>8) != rgb[2] {
			t.Errorf("code %d: top-left = (%d,%d,%d), want %v", code, r>>8, g>>8, bl>>8, rgb)
		}
	}

	// Orientation: packed index 0 is display (0,0) (bwry.Pack already applied the Y-mirror). A frame
	// that is code-3 (red) only in its first byte (top-left 4 px) must decode red at (0,0), white below.
	f := packCodeFrame(1) // all white
	f[0] = 3 << 6         // first pixel (display 0,0) = red, rest of the byte white
	raw, err := PackedToPNG(f)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(raw))
	if r, _, _, _ := img.At(0, 0).RGBA(); uint8(r>>8) != 255 {
		t.Errorf("orientation: (0,0) not red")
	}
	if r, g, b, _ := img.At(0, 10).RGBA(); uint8(r>>8) != 255 || uint8(g>>8) != 255 || uint8(b>>8) != 255 {
		t.Errorf("orientation: (0,10) not white (got %d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

// TestPackedToPNG_SizeGate rejects an off-size frame (the caller falls back), never a partial image.
func TestPackedToPNG_SizeGate(t *testing.T) {
	if _, err := PackedToPNG(make([]byte, 100)); err == nil {
		t.Fatal("want error on off-size frame")
	}
}
