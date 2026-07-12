// W2 gates (design 01-ota-spa §4.2/§7 W2): the client guard that must block an
// oversize / empty / over-length-version firmware blob BEFORE any request
// escapes, plus the sha256-injection and error-localization contracts. Pure node
// — no DOM; Muster lib/media/uploader.test.ts.
//
// N1/N2/N3 are the load-bearing negative probes: guardedFirmwareUpload must
// throw FwGuardError AND never call the injected upload (guard bypassed ⇒ the
// request would escape — the exact regression this pins). N2 additionally pins
// the UTF-8-BYTE length semantics (ota_http.go:61) against a UTF-16-code-unit
// `.length` regression. N4 proves the guard is load-bearing: within the caps,
// upload IS called with the injected hash's result in the sha256 field.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'
import {
  FwGuardError,
  MAX_FIRMWARE_BYTES,
  fwGuardText,
  guardedFirmwareUpload,
  otaErrorText,
} from './firmware'
import type { FirmwareRegistered } from './types'

function fileOf(bytes: number, name = 'fw.bin'): File {
  return new File([new Uint8Array(bytes)], name, { type: 'application/octet-stream' })
}

const ok: FirmwareRegistered = { success: true, version: '1.0.0', sha256: 'a'.repeat(64), size_bytes: 100 }

describe('guardedFirmwareUpload — the guard gates the request', () => {
  // N1 — 16-MB-Guard: blocks BEFORE the request; deps.upload never called.
  it('blocks a blob > 16 MB (upload never called)', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(
      guardedFirmwareUpload(fileOf(MAX_FIRMWARE_BYTES + 1), '1.0.0', { hash, upload }),
    ).rejects.toMatchObject({ name: 'FwGuardError', reason: 'too_large' })
    expect(hash).not.toHaveBeenCalled()
    expect(upload).not.toHaveBeenCalled()
  })

  it('admits a blob exactly at the 16 MB cap', async () => {
    const upload = vi.fn(async (form: FormData) => {
      expect(form.get('blob')).toBeInstanceOf(File)
      return ok
    })
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(
      guardedFirmwareUpload(fileOf(MAX_FIRMWARE_BYTES), '1.0.0', { hash, upload }),
    ).resolves.toEqual(ok)
    expect(upload).toHaveBeenCalledTimes(1)
  })

  // N2 — Byte-length guard (ota_http.go:61 `len(version) > 31`, BYTES). A version
  // of 29×'a' + 'ü' is 30 UTF-16 code units AND 31 UTF-8 bytes ('ü' = 2 bytes) —
  // the GREEN boundary case (exactly 31 bytes, admitted). 30×'a' + 'ü' is 31 code
  // units but 32 UTF-8 bytes — the RED-if-`.length` case: a naive `.length` guard
  // reads 31 (<=31, admitted) and would let it through; the byte-correct guard
  // reads 32 (>31) and rejects with 'bad_version'. This is the exact divergence
  // design 01-ota-spa §4.2 calls out (three masses: bytes/chars/UTF-16 units).
  it('admits a 31-UTF-8-byte multibyte version (30 chars, boundary GREEN)', async () => {
    const version = 'a'.repeat(29) + 'ü' // 29 + 1 = 30 UTF-16 units; 29 + 2 = 31 UTF-8 bytes
    expect(new TextEncoder().encode(version).length).toBe(31)
    const upload = vi.fn().mockResolvedValue(ok)
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(guardedFirmwareUpload(fileOf(100), version, { hash, upload })).resolves.toEqual(ok)
    expect(upload).toHaveBeenCalledTimes(1)
  })

  it('rejects a 32-UTF-8-byte multibyte version that a UTF-16 .length guard would admit (RED-if-.length)', async () => {
    const version = 'a'.repeat(30) + 'ü' // 30 + 1 = 31 UTF-16 units (<=31 → a .length guard admits it)
    expect(version.length).toBe(31) // a naive `.length` guard sees 31 and would NOT throw — the bug this pins
    expect(new TextEncoder().encode(version).length).toBe(32) // the byte-correct guard sees 32 and rejects
    const upload = vi.fn().mockResolvedValue(ok)
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(
      guardedFirmwareUpload(fileOf(100), version, { hash, upload }),
    ).rejects.toMatchObject({ name: 'FwGuardError', reason: 'bad_version' })
    expect(hash).not.toHaveBeenCalled()
    expect(upload).not.toHaveBeenCalled()
  })

  it('rejects an empty version string', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(guardedFirmwareUpload(fileOf(100), '', { hash, upload })).rejects.toMatchObject({
      reason: 'bad_version',
    })
    expect(upload).not.toHaveBeenCalled()
  })

  // N3 — empty file.
  it('blocks a zero-byte file (upload never called)', async () => {
    const upload = vi.fn().mockResolvedValue(ok)
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    await expect(guardedFirmwareUpload(fileOf(0), '1.0.0', { hash, upload })).rejects.toMatchObject({
      name: 'FwGuardError',
      reason: 'empty',
    })
    expect(hash).not.toHaveBeenCalled()
    expect(upload).not.toHaveBeenCalled()
  })

  // N4 — sha-injection: the injected hash runs against the file bytes and its
  // result lands in the FormData `sha256` field.
  it('calls the injected hash with the file bytes and puts its result in sha256', async () => {
    const bytes = new Uint8Array([1, 2, 3, 4])
    const file = new File([bytes], 'fw.bin', { type: 'application/octet-stream' })
    const hash = vi.fn(async (b: Uint8Array) => {
      expect([...b]).toEqual([1, 2, 3, 4]) // hashed exactly the file's bytes
      return 'deadbeef'.repeat(8) // 64 hex chars
    })
    const upload = vi.fn(async (form: FormData) => {
      expect(form.get('sha256')).toBe('deadbeef'.repeat(8)) // the hash's result, not a server-side recompute
      expect(form.get('version')).toBe('2.0.0')
      expect(form.get('blob')).toBe(file)
      return ok
    })
    await guardedFirmwareUpload(file, '2.0.0', { hash, upload })
    expect(hash).toHaveBeenCalledTimes(1)
    expect(upload).toHaveBeenCalledTimes(1)
  })

  it('reports upload progress through the injected onProgress', async () => {
    const upload = vi.fn(async (_form: FormData, onProgress?: (f: number) => void) => {
      onProgress?.(0.5)
      onProgress?.(1)
      return ok
    })
    const hash = vi.fn().mockResolvedValue('a'.repeat(64))
    const seen: number[] = []
    await guardedFirmwareUpload(fileOf(100), '1.0.0', { hash, upload, onProgress: (f) => seen.push(f) })
    expect(seen).toEqual([0.5, 1])
  })
})

describe('fwGuardText — every guard reason maps to a localized ota.fw.error.* string', () => {
  it('maps every reason to a non-empty string', () => {
    for (const r of ['empty', 'too_large', 'bad_version'] as const) {
      expect(fwGuardText(r)).toMatch(/\S/)
    }
  })
})

describe('otaErrorText — keys on the server envelope code, not the HTTP status', () => {
  it('distinguishes duplicate_version (409) from sha_mismatch (422)', () => {
    const duplicate = new ApiError(409, 'conflict', 'raw', null, { code: 'duplicate_version' })
    const mismatch = new ApiError(422, 'validation', 'raw', null, { code: 'sha_mismatch' })
    expect(otaErrorText(duplicate)).not.toBe(otaErrorText(mismatch))
  })

  it('maps invalid_sha / invalid_version / blob_too_large to distinct strings', () => {
    const invalidSha = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_sha' })
    const invalidVersion = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_version' })
    const tooLarge = new ApiError(422, 'validation', 'raw', null, { code: 'blob_too_large' })
    const texts = [otaErrorText(invalidSha), otaErrorText(invalidVersion), otaErrorText(tooLarge)]
    expect(new Set(texts).size).toBe(3) // all three distinct
  })

  it('falls back to e.message for a code without a dedicated ota string', () => {
    const e = new ApiError(500, 'server', 'boom', null, { code: 'internal' })
    expect(otaErrorText(e)).toBe('boom')
  })

  it('falls back to e.code when details carries no string code', () => {
    const e = new ApiError(0, 'network', 'admin API unreachable')
    expect(otaErrorText(e)).toBe('admin API unreachable')
  })
})
