// W2 gates (design 02-settings-spa §4/§5): the client-side name-regex mirror and
// the error-code → i18n mapping. Pure node — no DOM; Muster lib/ota/firmware.test.ts.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { isValidSecretName, settingsErrorText } from './secrets'

describe('isValidSecretName — client mirror of secrets.go:32 validName (cosmetic, server stays authoritative)', () => {
  it('accepts names matching ^[a-z0-9][a-z0-9._-]{0,127}$', () => {
    expect(isValidSecretName('a')).toBe(true)
    expect(isValidSecretName('grafana.api_key-1')).toBe(true)
    expect(isValidSecretName('0-leading-digit')).toBe(true)
    expect(isValidSecretName('a'.repeat(128))).toBe(true) // 128 chars, the cap
  })

  it('rejects an empty string, uppercase, illegal chars and a leading illegal char', () => {
    expect(isValidSecretName('')).toBe(false)
    expect(isValidSecretName('Grafana')).toBe(false) // uppercase not permitted
    expect(isValidSecretName('has space')).toBe(false)
    expect(isValidSecretName('has/slash')).toBe(false)
    expect(isValidSecretName('.leading-dot')).toBe(false) // must start [a-z0-9]
    expect(isValidSecretName('-leading-hyphen')).toBe(false)
  })

  it('rejects a name one byte over the 128-char cap', () => {
    expect(isValidSecretName('a'.repeat(129))).toBe(false)
  })
})

describe('settingsErrorText — keys on the server envelope code, not the HTTP status (Muster otaErrorText)', () => {
  it('maps invalid_name / empty_value / value_too_large to distinct, non-empty strings', () => {
    const invalidName = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_name' })
    const emptyValue = new ApiError(422, 'validation', 'raw', null, { code: 'empty_value' })
    const tooLarge = new ApiError(413, 'validation', 'raw', null, { code: 'value_too_large' })
    const texts = [settingsErrorText(invalidName), settingsErrorText(emptyValue), settingsErrorText(tooLarge)]
    for (const t of texts) expect(t).toMatch(/\S/)
    expect(new Set(texts).size).toBe(3) // all three distinct
  })

  it('maps not_found (DELETE/GET 404) to its own localized string', () => {
    const notFound = new ApiError(404, 'not_found', 'raw', null, { code: 'not_found' })
    expect(settingsErrorText(notFound)).toMatch(/\S/)
  })

  it('falls back to e.message for a code without a dedicated settings string', () => {
    const e = new ApiError(500, 'server', 'boom', null, { code: 'internal' })
    expect(settingsErrorText(e)).toBe('boom')
  })

  it('falls back to e.code when details carries no string code', () => {
    const e = new ApiError(0, 'network', 'admin API unreachable')
    expect(settingsErrorText(e)).toBe('admin API unreachable')
  })

  // B2 — the write-only invariant this function must never violate: it takes
  // only an error, never a value; nothing about its inputs/outputs can carry a
  // secret's plaintext. Pinned structurally: the function signature admits no
  // second (value) argument.
  it('takes a single argument (no value/plaintext channel into the error text)', () => {
    expect(settingsErrorText.length).toBe(1)
  })
})
