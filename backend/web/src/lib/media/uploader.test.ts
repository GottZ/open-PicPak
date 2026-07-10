// W4 gates (design 29 §4.4/§5, §7 W4): the client guard that must block a
// decode-bomb / oversize / unsupported file BEFORE any request escapes, plus the
// approx-preview quantizer round-trip. Pure node — no DOM, no createImageBitmap;
// the browser decode is svelte-check-only, the DECISION logic is pinned here.
//
// The load-bearing negative probe is the dimension guard: guardedUpload with an
// over-cap probe must throw GuardError AND never call the injected upload (guard
// bypassed ⇒ the request would escape — the exact regression this pins). The
// paired happy-path test proves the guard is what gates: within the cap, upload
// IS called.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'
import { decodePacked, PACKED_SIZE } from '../faas/bwrydecode'
import {
  GuardError,
  MAX_IMAGE_PIXELS,
  MAX_UPLOAD_BYTES,
  guardedUpload,
  guardText,
  preCheck,
  quantizeNearest,
  readImageDimensions,
  uploadErrorText,
  withinPixelCap,
} from './uploader'
import type { UploadedImage } from './types'

function pngHeader(width: number, height: number): Uint8Array {
  const b = new Uint8Array(24)
  b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a], 0) // signature
  b.set([0x00, 0x00, 0x00, 0x0d], 8) // IHDR length
  b.set([0x49, 0x48, 0x44, 0x52], 12) // "IHDR"
  b[16] = (width >>> 24) & 0xff
  b[17] = (width >>> 16) & 0xff
  b[18] = (width >>> 8) & 0xff
  b[19] = width & 0xff
  b[20] = (height >>> 24) & 0xff
  b[21] = (height >>> 16) & 0xff
  b[22] = (height >>> 8) & 0xff
  b[23] = height & 0xff
  return b
}

function fileOf(bytes: number, type: string): File {
  // A File with a controlled .size (never actually read in the guard tests that
  // inject a probe) — Blob padding to the requested byte length.
  return new File([new Uint8Array(bytes)], 'x', { type })
}

describe('readImageDimensions', () => {
  it('parses a PNG IHDR', () => {
    expect(readImageDimensions(pngHeader(400, 300))).toEqual({ width: 400, height: 300 })
  })

  it('parses a GIF logical screen (little-endian)', () => {
    const b = new Uint8Array([0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x90, 0x01, 0x2c, 0x01])
    expect(readImageDimensions(b)).toEqual({ width: 400, height: 300 })
  })

  it('parses a JPEG SOF0 marker', () => {
    const b = new Uint8Array([0xff, 0xd8, 0xff, 0xc0, 0x00, 0x11, 0x08, 0x01, 0x2c, 0x01, 0x90, 0x00])
    expect(readImageDimensions(b)).toEqual({ width: 400, height: 300 })
  })

  it('walks past a preceding APP0 segment to the SOF', () => {
    const b = new Uint8Array([
      0xff, 0xd8, // SOI
      0xff, 0xe0, 0x00, 0x04, 0x41, 0x42, // APP0, len 4, 2 payload bytes
      0xff, 0xc0, 0x00, 0x11, 0x08, 0x01, 0x2c, 0x01, 0x90, 0x00, // SOF0 300×400
    ])
    expect(readImageDimensions(b)).toEqual({ width: 400, height: 300 })
  })

  it('returns null for an unrecognised / truncated header', () => {
    expect(readImageDimensions(new Uint8Array([0x00, 0x01, 0x02, 0x03]))).toBeNull()
  })
})

describe('preCheck + withinPixelCap', () => {
  it('rejects a zero-byte file', () => {
    expect(preCheck({ size: 0, type: 'image/png' })).toBe('not_an_image')
  })
  it('rejects an oversize file before any decode', () => {
    expect(preCheck({ size: MAX_UPLOAD_BYTES + 1, type: 'image/png' })).toBe('too_large')
  })
  it('rejects an unsupported mime (e.g. webp the store cannot decode)', () => {
    expect(preCheck({ size: 100, type: 'image/webp' })).toBe('unsupported_type')
  })
  it('admits a valid png within the caps', () => {
    expect(preCheck({ size: 100, type: 'image/png' })).toBeNull()
  })
  it('flags an over-cap pixel area', () => {
    expect(withinPixelCap(25000, 25000)).toBe(false) // 625M ≫ 24M
    expect(withinPixelCap(400, 300)).toBe(true)
  })
})

describe('guardedUpload — the guard gates the request', () => {
  const ok: UploadedImage = {
    success: true,
    id: 7,
    sha256: 'abc',
    mime: 'image/png',
    width: 400,
    height: 300,
    byte_size: 100,
    created_at: '2026-07-10T00:00:00Z',
  }

  it('blocks an over-dimension image BEFORE the request (upload never called)', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const probe = vi.fn().mockResolvedValue({ width: 25000, height: 25000 }) // > MAX_IMAGE_PIXELS
    await expect(guardedUpload(fileOf(100, 'image/png'), { probe, upload })).rejects.toMatchObject({
      name: 'GuardError',
      reason: 'over_dimension',
    })
    expect(upload).not.toHaveBeenCalled() // the request never escaped
  })

  it('blocks an undecodable file (probe throws) without a request', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const probe = vi.fn().mockRejectedValue(new Error('undecodable'))
    await expect(guardedUpload(fileOf(100, 'image/png'), { probe, upload })).rejects.toMatchObject({
      reason: 'not_an_image',
    })
    expect(upload).not.toHaveBeenCalled()
  })

  it('blocks an oversize file before the probe even runs', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const probe = vi.fn()
    await expect(
      guardedUpload(fileOf(MAX_UPLOAD_BYTES + 1, 'image/png'), { probe, upload }),
    ).rejects.toMatchObject({ reason: 'too_large' })
    expect(probe).not.toHaveBeenCalled()
    expect(upload).not.toHaveBeenCalled()
  })

  it('proves the guard is load-bearing: within the cap, upload IS called with FormData{file} + progress', async () => {
    const upload = vi.fn(async (form: FormData, onProgress?: (f: number) => void) => {
      onProgress?.(0.5)
      onProgress?.(1)
      expect(form.get('file')).toBeInstanceOf(File)
      expect(form.has('name')).toBe(false) // the shipped API has no name field
      return ok
    })
    const seen: number[] = []
    const res = await guardedUpload(fileOf(100, 'image/png'), {
      probe: async () => ({ width: 400, height: 300 }),
      upload,
      onProgress: (f) => seen.push(f),
    })
    expect(upload).toHaveBeenCalledTimes(1)
    expect(seen).toEqual([0.5, 1])
    expect(res.id).toBe(7)
  })
})

describe('quantizeNearest — approx-preview inverse of decodePacked', () => {
  it('round-trips palette-quantized bytes through decode→quantize', () => {
    // A packed frame whose codes cycle 0,1,2,3 across every byte.
    const packed = new Uint8Array(PACKED_SIZE).fill(0b00011011)
    const rgba = decodePacked(packed) // → exact palette RGB
    const requantized = quantizeNearest(rgba) // nearest palette maps each colour to itself
    expect(requantized).toEqual(packed)
  })
})

describe('error localization', () => {
  it('guardText maps every reason to a media string', () => {
    for (const r of ['too_large', 'over_dimension', 'unsupported_type', 'not_an_image'] as const) {
      expect(guardText(r)).toMatch(/\S/)
    }
  })

  it('uploadErrorText keys on the server envelope code, not the HTTP status', () => {
    const unsupported = new ApiError(422, 'validation', 'raw', null, { code: 'unsupported_type' })
    const tooLarge = new ApiError(422, 'validation', 'raw', null, { code: 'blob_too_large' })
    // Distinct 422s resolve to distinct localized strings.
    expect(uploadErrorText(unsupported)).not.toBe(uploadErrorText(tooLarge))
  })

  it('falls back to the raw message for a code without a media string', () => {
    const e = new ApiError(500, 'server', 'boom', null, { code: 'internal' })
    expect(uploadErrorText(e)).toBe('boom')
  })
})
