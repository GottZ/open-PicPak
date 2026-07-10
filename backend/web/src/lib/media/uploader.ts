// Uploader logic (design 29 §4.4) — the pure, node-testable core the thin
// Uploader.svelte drives. Every guard runs BEFORE any network request; the DoS
// vectors (byte size, decode-bomb pixel dimensions) are rejected client-side so a
// hostile file never reaches the wire (§5 S8 byte-DoS, Finding 1 decode-bomb).
//
// Dimensions come from a PURE header parse (readImageDimensions) — the browser
// mirror of the server's image.DecodeConfig (imgstore/store.go), NOT a full
// createImageBitmap decode. This is the deliberate improvement over the design's
// createImageBitmap-resize probe: createImageBitmap does NOT stream into a bounded
// buffer (Finding 1), so reading native dimensions through it still materialises
// the bomb. Parsing the container header (PNG IHDR / JPEG SOF / GIF screen) reads
// width×height from a few bytes with no decode — the guard blocks a 25000×25000
// image before a single pixel is allocated.

import { ApiError, toApiError } from '../api'
import { PALETTE, PACKED_SIZE, W, H } from '../faas/bwrydecode'
import { m } from '../../paraglide/messages.js'
import type { UploadedImage } from './types'

// Mirrors of the server caps (image_http.go defaultMaxImageBytes, imgstore
// MaxImagePixels). Constant spiegel of a server limit, like NAME_RE in
// faas/functions.ts — kept in sync by the Go golden test / this comment.
export const MAX_UPLOAD_BYTES = 24 * 1024 * 1024 // 24 MiB
export const MAX_IMAGE_PIXELS = 24_000_000 // width × height

// The store decodes png/jpeg/gif only (imgstore/store.go formatMIME); webp is
// admitted by the HTTP allowlist but rejected 422 by the decoder. Offering only
// the decodable set avoids the wasted round-trip (briefing: handle webp cleanly).
export const ACCEPTED_MIME = ['image/png', 'image/jpeg', 'image/gif'] as const
export const ACCEPT_ATTR = ACCEPTED_MIME.join(',')

// Enough to cover any real image header (a JPEG SOF marker after a large EXIF/ICC
// block); the byte-cap check runs first, so this slice is always ≤ the 24 MiB cap.
export const HEADER_SLICE = 256 * 1024

export type GuardReason = 'unsupported_type' | 'too_large' | 'over_dimension' | 'not_an_image'

/** A client guard rejection — thrown BEFORE any upload request escapes. */
export class GuardError extends Error {
  readonly reason: GuardReason
  constructor(reason: GuardReason) {
    super(reason)
    this.name = 'GuardError'
    this.reason = reason
  }
}

/** Byte + MIME pre-checks that need no decode. Returns the reason, or null if OK. */
export function preCheck(file: { size: number; type: string }): GuardReason | null {
  if (file.size === 0) return 'not_an_image'
  if (file.size > MAX_UPLOAD_BYTES) return 'too_large'
  if (!ACCEPTED_MIME.includes(file.type as (typeof ACCEPTED_MIME)[number])) return 'unsupported_type'
  return null
}

/** True when the source dimensions are within the server pixel cap (§4.4). */
export function withinPixelCap(width: number, height: number): boolean {
  return width > 0 && height > 0 && width * height <= MAX_IMAGE_PIXELS
}

/**
 * Read native (width, height) from a PNG / JPEG / GIF container header — the
 * browser mirror of Go's image.DecodeConfig (header only, no pixel decode). Reads
 * from the leading bytes only; returns null for an unrecognised / truncated header
 * (the caller treats null as 'not_an_image'). This is what makes the decode-bomb
 * guard both safe (no full decode) and node-testable (pure, tiny fixtures).
 */
export function readImageDimensions(bytes: Uint8Array): { width: number; height: number } | null {
  // PNG: 8-byte signature, then IHDR — width @16 / height @20, big-endian u32.
  if (
    bytes.length >= 24 &&
    bytes[0] === 0x89 && bytes[1] === 0x50 && bytes[2] === 0x4e && bytes[3] === 0x47 &&
    bytes[4] === 0x0d && bytes[5] === 0x0a && bytes[6] === 0x1a && bytes[7] === 0x0a
  ) {
    return { width: readU32BE(bytes, 16), height: readU32BE(bytes, 20) }
  }
  // GIF: "GIF87a"/"GIF89a", logical screen width @6 / height @8, little-endian u16.
  if (
    bytes.length >= 10 &&
    bytes[0] === 0x47 && bytes[1] === 0x49 && bytes[2] === 0x46
  ) {
    return { width: bytes[6] | (bytes[7] << 8), height: bytes[8] | (bytes[9] << 8) }
  }
  // JPEG: SOI 0xFFD8, then walk marker segments to the first SOF (0xC0..0xCF,
  // excluding the non-frame markers C4/C8/CC) — height @ marker+5, width @+7 (BE).
  if (bytes.length >= 4 && bytes[0] === 0xff && bytes[1] === 0xd8) {
    let i = 2
    while (i + 9 < bytes.length) {
      if (bytes[i] !== 0xff) {
        i++ // skip fill / entropy bytes until the next marker
        continue
      }
      const marker = bytes[i + 1]
      if (marker === 0xd8 || marker === 0xd9 || (marker >= 0xd0 && marker <= 0xd7)) {
        i += 2 // standalone marker, no length
        continue
      }
      const segLen = (bytes[i + 2] << 8) | bytes[i + 3]
      const isSOF =
        marker >= 0xc0 && marker <= 0xcf && marker !== 0xc4 && marker !== 0xc8 && marker !== 0xcc
      if (isSOF) {
        return { height: readU16BE(bytes, i + 5), width: readU16BE(bytes, i + 7) }
      }
      i += 2 + segLen // skip this segment's payload
    }
  }
  return null
}

function readU32BE(b: Uint8Array, o: number): number {
  return (b[o] * 0x1000000) + (b[o + 1] << 16) + (b[o + 2] << 8) + b[o + 3]
}
function readU16BE(b: Uint8Array, o: number): number {
  return (b[o] << 8) | b[o + 1]
}

/** Injected transport + probe — lets the orchestration be node-tested without DOM. */
export interface UploadDeps {
  /** Native source dimensions; throws / rejects for an undecodable file. */
  probe: (file: File) => Promise<{ width: number; height: number }>
  /** The multipart transport (apiUpload) — only reached after every guard passes. */
  upload: (form: FormData, onProgress?: (frac: number) => void) => Promise<UploadedImage>
  onProgress?: (frac: number) => void
}

/**
 * The DoS-safe upload path (§4.4): pre-check → dimension guard → upload. A guard
 * breach throws GuardError and NEVER calls deps.upload — the W4 negative probe
 * pins exactly this (guard bypassed ⇒ the request escapes). The FormData carries
 * only `file` (the shipped API has no `name` field, image_http.go r.FormFile).
 */
export async function guardedUpload(file: File, deps: UploadDeps): Promise<UploadedImage> {
  const pre = preCheck(file)
  if (pre) throw new GuardError(pre)

  let dims: { width: number; height: number }
  try {
    dims = await deps.probe(file)
  } catch {
    throw new GuardError('not_an_image')
  }
  if (!withinPixelCap(dims.width, dims.height)) throw new GuardError('over_dimension')

  const form = new FormData()
  form.append('file', file)
  return deps.upload(form, deps.onProgress)
}

/** Localized text for a client guard rejection (design A34 §2 — client localizes). */
export function guardText(reason: GuardReason): string {
  switch (reason) {
    case 'too_large':
      return m['media.upload.error.too_large']()
    case 'over_dimension':
      return m['media.upload.error.over_dimension']()
    case 'unsupported_type':
      return m['media.upload.error.unsupported']()
    case 'not_an_image':
      return m['media.upload.error.not_an_image']()
  }
}

/**
 * Localized text for a server upload rejection, keyed on the envelope's machine
 * code (image_http.go) rather than the HTTP status — so a 422 unsupported_type is
 * distinguishable from a 422 blob_too_large. Falls back to the raw ApiError
 * message for codes without a dedicated media string.
 */
export function uploadErrorText(err: unknown): string {
  const e = toApiError(err)
  const serverCode = typeof e.details?.['code'] === 'string' ? (e.details['code'] as string) : e.code
  switch (serverCode) {
    case 'unsupported_type':
      return m['media.upload.error.unsupported']()
    case 'blob_too_large':
      return m['media.upload.error.too_large']()
    case 'missing_file':
      return m['media.upload.error.not_an_image']()
    default:
      return e.message || m['media.upload.error.failed']()
  }
}

// ---- Approx-Preview (design §4.4, §8 E-A29-1) ------------------------------------
// Best-effort, deliberately NOT bit-exact (S7): the immediate client preview while
// the server pack round-trips. The authoritative WYSIWYG is always decodePacked of
// the server's 30000 bytes — this is a nearest-palette approximation only.

/** Nearest BWRY palette code (argmin squared distance) for one RGB pixel. */
function nearestCode(r: number, g: number, b: number): number {
  let best = 0
  let bestD = Infinity
  for (let c = 0; c < 4; c++) {
    const [pr, pg, pb] = PALETTE[c]
    const d = (r - pr) * (r - pr) + (g - pg) * (g - pg) + (b - pb) * (b - pb)
    if (d < bestD) {
      bestD = d
      best = c
    }
  }
  return best
}

/**
 * Quantize a 400×300 RGBA buffer (from the resized bitmap) into the same 30000-byte
 * packed BWRY layout decodePacked consumes — the deterministic INVERSE of the
 * decoder: nearest palette, panel Y-mirror (packed row p ← display row H-1-p), 2bpp
 * MSB-first 4px/byte. Feeding the result back through decodePacked reproduces the
 * palette-quantized colours (round-trip test). Pure → node-testable.
 */
export function quantizeNearest(rgba: Uint8ClampedArray): Uint8Array {
  const packed = new Uint8Array(PACKED_SIZE)
  for (let p = 0; p < H; p++) {
    const dispRow = H - 1 - p // re-apply the panel Y-mirror the decoder un-flips
    for (let x = 0; x < W; x++) {
      const src = (dispRow * W + x) * 4
      const code = nearestCode(rgba[src], rgba[src + 1], rgba[src + 2])
      const px = p * W + x
      packed[px >> 2] |= code << (6 - 2 * (px & 3))
    }
  }
  return packed
}

// Re-export so the component imports the panel geometry from one place.
export { W, H, ApiError }
