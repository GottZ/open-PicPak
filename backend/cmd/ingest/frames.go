package main

import _ "embed"

// unavailableFrame is served on the /frame path when the faas-supervisor is unreachable or returns a
// non-frame body (D24 §4.3 / T13): a valid 30000-byte BWRY frame, so the panel shows a recognisable
// "backend unavailable" graphic — never a blank/garbage frame nor a 500 to the device. It is a STATIC
// build asset (fix-5); ingest links NO renderer/pack code (the pack is not device-parser code, D24.1),
// so the frame is embedded pre-packed rather than rendered here.
//
//go:embed unavailable.bin
var unavailableFrame []byte

// packedFrameSize is the fixed BWRY frame size (400×300 @ 2bpp); the supervisor guarantees it, ingest
// re-checks it before letting a body reach the panel.
const packedFrameSize = 30000

func init() {
	if len(unavailableFrame) != packedFrameSize {
		panic("ingest: embedded unavailable.bin must be exactly 30000 bytes")
	}
}
