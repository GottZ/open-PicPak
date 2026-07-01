import { describe, it, expect } from 'vitest'
import { decodePacked, rawRGBToRGBA, PALETTE, W, H, PACKED_SIZE, RAW_RGB_SIZE } from './bwrydecode'

// A faithful TS mirror of the Go bwry pack (quantizeFlipped + pack2bpp) — the golden the decode must invert
// (T6). codesUpright is W*H codes in UPRIGHT row-major (row 0 = top); the pack applies the panel Y-mirror.
function packUpright(codesUpright: Uint8Array): Uint8Array {
  const flipped = new Uint8Array(W * H)
  for (let y = 0; y < H; y++) {
    for (let x = 0; x < W; x++) flipped[y * W + x] = codesUpright[(H - 1 - y) * W + x]
  }
  const out = new Uint8Array(PACKED_SIZE)
  for (let n = 0; n < flipped.length; n += 4) {
    let v = 0
    for (let i = 0; i < 4; i++) v |= (flipped[n + i] & 3) << (6 - 2 * i)
    out[n >> 2] = v
  }
  return out
}

// T6 — the decode is the EXACT inverse of the pack: a known code pattern, packed (Y-flipped, 2bpp MSB),
// then decoded, recovers the UPRIGHT palette pixels at every position. Red: a "looks-right" decode silently
// misrenders the panel preview.
describe('decodePacked — golden inverse of the pack (T6)', () => {
  it('recovers the upright frame pixel-for-pixel', () => {
    const codes = new Uint8Array(W * H)
    for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) codes[y * W + x] = (x * 7 + y * 3) % 4
    const rgba = decodePacked(packUpright(codes))
    // spot-check a deterministic spread of pixels (full-frame compare is O(120k) — sample corners + middle)
    for (const [x, y] of [
      [0, 0],
      [399, 0],
      [0, 299],
      [399, 299],
      [200, 150],
      [1, 298],
      [398, 1],
    ]) {
      const code = codes[y * W + x]
      const o = (y * W + x) * 4
      expect([rgba[o], rgba[o + 1], rgba[o + 2]], `px ${x},${y} code ${code}`).toEqual(PALETTE[code])
      expect(rgba[o + 3]).toBe(255)
    }
  })

  it('un-flips: a mark in packed row 0 lands at the BOTTOM display row, not the top', () => {
    // upright frame: top row (y=0) all W(1), everything else K(0).
    const codes = new Uint8Array(W * H) // all 0 (K)
    for (let x = 0; x < W; x++) codes[x] = 1 // upright top row = white
    const rgba = decodePacked(packUpright(codes))
    // display top row (y=0) must be white; display bottom row (y=299) must be black.
    expect([rgba[0], rgba[1], rgba[2]]).toEqual([255, 255, 255])
    const bottom = (299 * W + 0) * 4
    expect([rgba[bottom], rgba[bottom + 1], rgba[bottom + 2]]).toEqual([0, 0, 0])
  })

  it('reads MSB-first: byte 0x1B → codes [0,1,2,3] (K,W,Y,R) across the first 4 packed pixels', () => {
    // packed pixel 0..3 are packed row 0 → display row 299. 0x1B = 00 01 10 11.
    const packed = new Uint8Array(PACKED_SIZE)
    packed[0] = 0x1b
    const rgba = decodePacked(packed)
    for (let x = 0; x < 4; x++) {
      const o = (299 * W + x) * 4
      expect([rgba[o], rgba[o + 1], rgba[o + 2]], `px ${x}`).toEqual(PALETTE[x]) // K,W,Y,R
    }
  })

  it('rejects an off-size packed buffer', () => {
    expect(() => decodePacked(new Uint8Array(100))).toThrow()
  })
})

// The palette is the internal/bwry order (0=K 1=W 2=Y 3=R) — a swapped order is a T6 negative probe.
describe('PALETTE', () => {
  it('is K, W, Y, R', () => {
    expect(PALETTE).toEqual([
      [0, 0, 0],
      [255, 255, 255],
      [255, 255, 0],
      [255, 0, 0],
    ])
  })
})

describe('rawRGBToRGBA', () => {
  it('expands RGB to opaque RGBA, no un-flip (the raw is already upright)', () => {
    const raw = new Uint8Array(RAW_RGB_SIZE)
    raw[0] = 10
    raw[1] = 20
    raw[2] = 30
    const rgba = rawRGBToRGBA(raw)
    expect([rgba[0], rgba[1], rgba[2], rgba[3]]).toEqual([10, 20, 30, 255])
    expect(rgba.length).toBe(W * H * 4)
  })
  it('rejects off-size raw', () => {
    expect(() => rawRGBToRGBA(new Uint8Array(10))).toThrow()
  })
})
