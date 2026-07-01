package bwry

// palette maps a BWRY code to its logical RGB colour (picpak palette PALETTE_CODES):
// 0=K 1=W 2=Y 3=R. Nearest mapping is against these LOGICAL colours (not the measured
// panel colours), matching the reference pipeline's quantize target.
var palette = [4][3]int32{
	{0, 0, 0},       // 0 = K (black)
	{255, 255, 255}, // 1 = W (white)
	{255, 255, 0},   // 2 = Y (yellow)
	{255, 0, 0},     // 3 = R (red)
}

// nearestCode returns the BWRY code minimising squared-Euclidean RGB distance to the
// logical palette; ties break to the lowest code index (strict-< argmin keeps the
// first minimum). This is EXACT nearest — see the package doc for the measured, and
// deliberate, deviation from PIL's cube-cache quantize(dither=NONE).
func nearestCode(r, g, b uint8) uint8 {
	best := uint8(0)
	bestD := int32(1)<<30 // > any 8-bit sq-distance (3*255^2 = 195075)
	rr, gg, bb := int32(r), int32(g), int32(b)
	for c := 0; c < 4; c++ {
		dr := rr - palette[c][0]
		dg := gg - palette[c][1]
		db := bb - palette[c][2]
		d := dr*dr + dg*dg + db*db
		if d < bestD {
			bestD = d
			best = uint8(c)
		}
	}
	return best
}
