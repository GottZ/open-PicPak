package faascore

import _ "embed"

// errorFrame is the stage-3 fallback: a pre-packed 30000-byte BWRY "render error" graphic served
// VERBATIM (fix-5) — no font/renderer dep in this binary, and the fallback never calls the worker.
//
//go:embed error.bin
var errorFrame []byte

func init() {
	if len(errorFrame) != 30000 {
		panic("faas-supervisor: embedded error.bin must be exactly 30000 bytes")
	}
}
