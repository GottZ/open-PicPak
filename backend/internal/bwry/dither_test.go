package bwry

import (
	"bytes"
	"testing"
)

// A31.1 (a) — Cross-conformance golden. PackWithDither(fixture, DitherAtkinson)
// must equal golden_atkinson.bin BYTE-FOR-BYTE. That golden is produced by the JS
// reference (testdata/gen_atkinson_golden.mjs, a verbatim copy of
// image-pipeline.md:116-155), NOT by the Go code — so a pass proves the Go engine
// reproduces the reference's exact bytes (bit-parity), not merely self-consistency.
func TestAtkinsonCrossConformanceGolden(t *testing.T) {
	fixture := loadTestdata(t, "fixture.bin")
	golden := loadTestdata(t, "golden_atkinson.bin")
	if len(golden) != PackedSize {
		t.Fatalf("golden len %d != %d", len(golden), PackedSize)
	}
	img, err := FromRGB(fixture)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}
	got, err := PackWithDither(img, DitherAtkinson)
	if err != nil {
		t.Fatalf("PackWithDither: %v", err)
	}
	if !bytes.Equal(got, golden) {
		diff := 0
		first := -1
		for i := range got {
			if got[i] != golden[i] {
				if first < 0 {
					first = i
				}
				diff++
			}
		}
		t.Fatalf("Atkinson golden mismatch: %d/%d bytes differ, first at %d: got %#02x want %#02x",
			diff, len(got), first, got[first], golden[first])
	}
}

// A31.1 (b) — dither=none regression. PackWithDither(_, DitherNone) and Pack MUST
// both stay byte-identical to the historical golden_none (A31-E2: no fleet
// regression). Also asserts Pack == PackWithDither(DitherNone) so the shared
// branch cannot drift, and that Atkinson actually DIVERGES from none (otherwise
// the none regression guard would be vacuous).
func TestDitherNoneByteIdentical(t *testing.T) {
	fixture := loadTestdata(t, "fixture.bin")
	goldenNone := loadTestdata(t, "golden_none.bin")
	img, err := FromRGB(fixture)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}

	viaNone, err := PackWithDither(img, DitherNone)
	if err != nil {
		t.Fatalf("PackWithDither(none): %v", err)
	}
	if !bytes.Equal(viaNone, goldenNone) {
		t.Error("PackWithDither(DitherNone) diverged from golden_none — fleet regression")
	}

	viaPack, err := Pack(img)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if !bytes.Equal(viaPack, viaNone) {
		t.Error("Pack diverged from PackWithDither(DitherNone) — shared none branch drift")
	}

	atk, err := PackWithDither(img, DitherAtkinson)
	if err != nil {
		t.Fatalf("PackWithDither(atkinson): %v", err)
	}
	if bytes.Equal(atk, goldenNone) {
		t.Error("Atkinson output equals golden_none — the dither path is a no-op (guard vacuous)")
	}
}

// A31.1 (c) — ParseDither fails closed. Only the implemented modes parse; every
// other string (typos, empty, reserved-but-unbuilt modes) is rejected so the
// admin layer can 422 rather than silently degrade to none (A31.3 groundwork).
func TestParseDitherFailClosed(t *testing.T) {
	ok := []struct {
		s    string
		want Dither
	}{
		{"none", DitherNone},
		{"atkinson", DitherAtkinson},
	}
	for _, tc := range ok {
		got, valid := ParseDither(tc.s)
		if !valid || got != tc.want {
			t.Errorf("ParseDither(%q) = (%d,%v), want (%d,true)", tc.s, got, valid, tc.want)
		}
	}
	bad := []string{"", "atknson", "Atkinson", "NONE", "floyd-steinberg", "ordered", "bogus", " none"}
	for _, s := range bad {
		if _, valid := ParseDither(s); valid {
			t.Errorf("ParseDither(%q) accepted — must fail closed", s)
		}
	}
}

// A31.1 determinism — the security-relevant property (EPD content-hash stability,
// §5): two runs of the Atkinson path over the same fixture produce identical
// bytes. Holds regardless of whether cross-conformance bit-parity holds.
func TestAtkinsonDeterministic(t *testing.T) {
	fixture := loadTestdata(t, "fixture.bin")
	img, err := FromRGB(fixture)
	if err != nil {
		t.Fatalf("FromRGB: %v", err)
	}
	a, err := PackWithDither(img, DitherAtkinson)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	b, err := PackWithDither(img, DitherAtkinson)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("Atkinson output non-deterministic across runs")
	}
}

// nearestCodeBT601: exact palette colours map to their own code, and the BT.601
// weighting diverges from the unweighted metric where the weights bite — e.g.
// pure green (0,255,0): unweighted nearest picks white/yellow by raw distance,
// but BT.601 keeps mid green from snapping the same way. Anchor a few known
// points from image-pipeline.md §3.
func TestNearestCodeBT601(t *testing.T) {
	for c := 0; c < 4; c++ {
		r, g, b := paletteF[c][0], paletteF[c][1], paletteF[c][2]
		if got := nearestCodeBT601(r, g, b); got != c {
			t.Errorf("nearestCodeBT601(exact palette %d) = %d, want %d", c, got, c)
		}
	}
	// Blue is always black (weight 0.114 is tiny) — image-pipeline.md:67.
	if got := nearestCodeBT601(0, 0, 255); got != 0 {
		t.Errorf("nearestCodeBT601(pure blue) = %d, want 0 (black)", got)
	}
}
