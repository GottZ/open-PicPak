// BWRY decode → RGBA (Design 25 §4.3 / D25.5) — the deterministic browser-side INVERSE of the supervisor's
// bwry pack, so the preview shows the EXACT bytes a device receives (the only preview that cannot lie about
// quantize / Y-flip). Pure (no worker, no eval) → fits the existing CSP and is node-testable (T6). Locked
// against the pack: pack2bpp is MSB-first 4px/byte, quantizeFlipped writes output row y from source row
// H-1-y (panel Y-mirror), palette 0=K 1=W 2=Y 3=R (internal/bwry/{pack,quantize}.go).

export const W = 400
export const H = 300
export const PACKED_SIZE = (W * H) / 4 // 30000
export const RAW_RGB_SIZE = W * H * 3 // 360000

// PALETTE_CODES (internal/bwry/quantize.go): index = BWRY code, value = logical RGB.
export const PALETTE: readonly [number, number, number][] = [
  [0, 0, 0], // 0 = K (black)
  [255, 255, 255], // 1 = W (white)
  [255, 255, 0], // 2 = Y (yellow)
  [255, 0, 0], // 3 = R (red)
]

/**
 * Decode a 30000-byte packed BWRY frame into an upright RGBA buffer (W*H*4) for a <canvas> ImageData.
 * Two inverses, both load-bearing:
 *  - 2bpp MSB-first unpack: code = (byte >> (6 - 2*(i&3))) & 3, 4 px/byte (inverse of pack2bpp).
 *  - un-flip: the packed frame is Y-mirrored (pack wrote output row y from source row H-1-y), so packed
 *    row p maps to DISPLAY row H-1-p — restoring the upright frame the operator drew.
 */
export function decodePacked(packed: Uint8Array): Uint8ClampedArray {
  if (packed.length !== PACKED_SIZE) {
    throw new Error(`bwrydecode: packed ${packed.length} != ${PACKED_SIZE}`)
  }
  const rgba = new Uint8ClampedArray(W * H * 4)
  for (let p = 0; p < H; p++) {
    const dispRow = H - 1 - p // un-flip the panel Y-mirror
    for (let x = 0; x < W; x++) {
      const px = p * W + x
      const code = (packed[px >> 2] >> (6 - 2 * (px & 3))) & 3
      const [r, g, b] = PALETTE[code]
      const o = (dispRow * W + x) * 4
      rgba[o] = r
      rgba[o + 1] = g
      rgba[o + 2] = b
      rgba[o + 3] = 255
    }
  }
  return rgba
}

/**
 * Convert the worker's raw pre-pack RGB (W*H*3, upright top-to-bottom — NOT Y-flipped: this is what the
 * function drew, before the pack) to RGBA for the "raw render" side-by-side canvas (D25.5). No un-flip.
 */
export function rawRGBToRGBA(raw: Uint8Array): Uint8ClampedArray {
  if (raw.length !== RAW_RGB_SIZE) {
    throw new Error(`bwrydecode: raw ${raw.length} != ${RAW_RGB_SIZE}`)
  }
  const rgba = new Uint8ClampedArray(W * H * 4)
  for (let i = 0, o = 0; i < raw.length; i += 3, o += 4) {
    rgba[o] = raw[i]
    rgba[o + 1] = raw[i + 1]
    rgba[o + 2] = raw[i + 2]
    rgba[o + 3] = 255
  }
  return rgba
}
