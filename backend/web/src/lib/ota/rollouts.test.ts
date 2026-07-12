// W4 gates (design 01-ota-spa §4.4/§7 W4): the rollout pure-core — 404-gone
// branch logic and the client serial-validation mirror. Pure node, Muster
// lib/ota/firmware.test.ts (compares against the actual m[...] message, never a
// hardcoded locale string).
//
// N3 — 404-gone: a rollout PATCH/DELETE hitting a row another operator already
// deleted must map to ota.rollout.error.gone, NOT otaErrorText's generic
// ota.channel.error.not_found (which is correct for channels, wrong here).
// N4 — the serial regex spiegelt ota_http.go:168-175 exactly.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { isRolloutGone, isValidRolloutSerial, rolloutErrorText } from './rollouts'
import { m } from '../../paraglide/messages.js'

describe('isRolloutGone (§4.4 404-gone concurrency edge)', () => {
  it('is true for a 404 status', () => {
    expect(isRolloutGone(new ApiError(404, 'not_found', 'no such rollout'))).toBe(true)
  })

  it('is true when the envelope code is not_found even off a non-404 status (belt-and-suspenders)', () => {
    expect(isRolloutGone(new ApiError(200, 'api_error', 'x', null, { code: 'not_found' }))).toBe(true)
  })

  it('is false for an unrelated failure', () => {
    expect(isRolloutGone(new ApiError(422, 'unknown_serial', 'no such device'))).toBe(false)
  })
})

describe("rolloutErrorText (§4.4) — the 404-gone branch must win over otaErrorText's generic not_found mapping", () => {
  // N3 — RED without the isRolloutGone() short-circuit: a naive `rolloutErrorText = otaErrorText`
  // would route a 404 through otaErrorText's `case 'not_found'`, which returns
  // ota.channel.error.not_found ("Channel nicht gefunden") — the wrong message for a
  // deleted rollout row. This assertion pins the CORRECT key and would fail red against
  // that naive implementation.
  it('maps a 404 to ota.rollout.error.gone, not the channel not_found string', () => {
    const text = rolloutErrorText(new ApiError(404, 'not_found', 'no such rollout'))
    expect(text).toBe(m['ota.rollout.error.gone']())
    expect(text).not.toBe(m['ota.channel.error.not_found']())
  })

  it('routes unknown_serial through the shared otaErrorText helper', () => {
    const text = rolloutErrorText(
      new ApiError(422, 'invalid_serial', 'no such device', null, { code: 'unknown_serial' }),
    )
    expect(text).toBe(m['ota.rollout.error.unknown_serial']())
  })

  it('routes unknown_channel_or_version to the unknown_target key', () => {
    const text = rolloutErrorText(
      new ApiError(422, 'validation', 'x', null, { code: 'unknown_channel_or_version' }),
    )
    expect(text).toBe(m['ota.rollout.error.unknown_target']())
  })
})

describe('isValidRolloutSerial (§4.4, mirrors ota_http.go:168-175 exactly)', () => {
  it('accepts the fleet wildcard and a valid device serial', () => {
    expect(isValidRolloutSerial('*')).toBe(true)
    expect(isValidRolloutSerial('ABC-12_3')).toBe(true)
  })

  // N4 — the negative set: empty, over-length (32 chars — the server caps at 31),
  // a space (not in the charset) and a non-ASCII char (ditto) must all be rejected.
  it('rejects empty, 32-char, space-containing and non-ASCII serials', () => {
    expect(isValidRolloutSerial('')).toBe(false)
    expect(isValidRolloutSerial('a'.repeat(32))).toBe(false)
    expect(isValidRolloutSerial('a b')).toBe(false)
    expect(isValidRolloutSerial('ä')).toBe(false)
  })

  it('accepts the 31-char boundary', () => {
    expect(isValidRolloutSerial('a'.repeat(31))).toBe(true)
  })
})
