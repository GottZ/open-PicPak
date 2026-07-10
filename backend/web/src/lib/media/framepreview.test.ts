import { describe, it, expect } from 'vitest'
import { decodePacked, W, H, PACKED_SIZE } from '../faas/bwrydecode'

// Canvas-Decode-Probe — <FramePreview> W3 gate (design 29 §7 W3). The component paints
// decodePacked(packed) → putImageData; that canvas path is browser-only (vite test env = node, §2), so the
// gate pins the DECODE the panel shows: known packed bytes ⇒ the exact RGBA pixels. It is the media-area
// pin on the shared BWRY decoder FramePreview depends on (complements bwrydecode.test.ts at this boundary).
// The expected pixels are LITERAL RGBA (not the imported PALETTE) on purpose — a self-referential PALETTE[x]
// expectation would move together with the decoder under a palette swap and catch nothing. With literals the
// probe is non-vacuous: a wrong palette, bit-order or Y-flip changes these pixels and fails the asserts
// (belegt in the W3 report by deliberately mutating the palette → red).
describe('FramePreview canvas-decode probe (design 29 §7 W3)', () => {
  it('paints 0x1B (MSB-first 00 01 10 11) as K,W,Y,R — and un-flips packed row 0 to the bottom display row', () => {
    const packed = new Uint8Array(PACKED_SIZE)
    packed[0] = 0x1b // 00 01 10 11 → codes 0,1,2,3 for the first four packed pixels
    const rgba = decodePacked(packed)
    // packed row 0 un-flips to display row H-1 (panel Y-mirror) — the exact literal pixels the canvas shows.
    const expected = [
      [0, 0, 0, 255], // code 0 = K
      [255, 255, 255, 255], // code 1 = W
      [255, 255, 0, 255], // code 2 = Y
      [255, 0, 0, 255], // code 3 = R
    ]
    for (let x = 0; x < 4; x++) {
      const o = ((H - 1) * W + x) * 4
      expect([rgba[o], rgba[o + 1], rgba[o + 2], rgba[o + 3]], `px ${x}`).toEqual(expected[x])
    }
    // and everything else stays black (K) — packed byte 0 only set the first 4 pixels.
    expect([rgba[0], rgba[1], rgba[2], rgba[3]]).toEqual([0, 0, 0, 255])
  })
})
