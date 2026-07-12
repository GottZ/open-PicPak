// W4 gates (design 02-settings-spa §7-W4): the mint-form pure-guard, the datetime-local→RFC3339
// conversion, and the error-code → i18n/field mapping. Pure node — no DOM; Muster
// lib/settings/secrets.test.ts.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import {
  KNOWN_SCOPES,
  mintFormValid,
  datetimeLocalToRFC3339,
  tokenErrorField,
  tokensErrorText,
  tokenStatusText,
} from './tokens'

// N3 — Scope-Pflicht: leere Scopes → invalid; Submit-Guard darf niemals ohne Label ODER ohne
// mindestens einen Scope grün gehen (§7-W4 "Submit disabled ohne Label oder ohne ≥1 Scope").
describe('mintFormValid — Pflicht-Label + mindestens ein Scope (N3, §7-W4 Submit-Guard)', () => {
  it('rejects an empty label even with scopes selected', () => {
    expect(mintFormValid({ label: '', scopes: ['image:read'] })).toBe(false)
  })

  it('rejects a whitespace-only label', () => {
    expect(mintFormValid({ label: '   ', scopes: ['image:read'] })).toBe(false)
  })

  it('rejects a label with zero scopes selected — the load-bearing negative probe', () => {
    expect(mintFormValid({ label: 'ci-pipeline', scopes: [] })).toBe(false)
  })

  it('accepts a non-empty label with at least one KNOWN_SCOPES entry', () => {
    for (const s of KNOWN_SCOPES) {
      expect(mintFormValid({ label: 'ci-pipeline', scopes: [s] })).toBe(true)
    }
  })

  it('accepts a non-empty label with both scopes selected', () => {
    expect(mintFormValid({ label: 'ci-pipeline', scopes: [...KNOWN_SCOPES] })).toBe(true)
  })

  it('KNOWN_SCOPES mirrors apitoken.KnownScopes (store.go:51-52) exactly — image:read + image:write', () => {
    expect([...KNOWN_SCOPES].sort()).toEqual(['image:read', 'image:write'])
  })
})

// N4 — datetime-local → RFC3339: leer → undefined/weggelassen; gesetzt → gültiges RFC3339, das der Go-
// Parser (time.RFC3339) nimmt.
describe('datetimeLocalToRFC3339 (N4, token_http.go:54 time.Parse(time.RFC3339, …))', () => {
  it('an empty string means "kein Ablauf" → undefined (never an empty/invalid timestamp)', () => {
    expect(datetimeLocalToRFC3339('')).toBeUndefined()
  })

  it('a whitespace-only string also means "kein Ablauf" → undefined', () => {
    expect(datetimeLocalToRFC3339('   ')).toBeUndefined()
  })

  it('a garbage string that Date cannot parse → undefined, not a malformed RFC3339', () => {
    expect(datetimeLocalToRFC3339('not-a-date')).toBeUndefined()
  })

  it('a set datetime-local value converts to a well-formed RFC3339 string', () => {
    const rfc = datetimeLocalToRFC3339('2026-08-01T12:30')
    expect(rfc).toBeDefined()
    // RFC3339 / time.RFC3339 shape: YYYY-MM-DDTHH:mm:ssZ (this fn always emits the Z-suffixed UTC form).
    expect(rfc).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/)
  })

  it('the produced RFC3339 string round-trips through Date.parse (what a Go time.Parse(time.RFC3339) would also accept)', () => {
    const rfc = datetimeLocalToRFC3339('2026-08-01T12:30:45')!
    const reparsed = new Date(rfc)
    expect(Number.isNaN(reparsed.getTime())).toBe(false)
  })

  it('seconds are preserved when present in the input', () => {
    const rfc = datetimeLocalToRFC3339('2026-08-01T12:30:45')!
    expect(rfc.endsWith(':45Z')).toBe(true)
  })
})

// Fehlerabbildung — Gate a: 422 unknown_scope muss als Scope-Feld-Fehler adressierbar sein, nicht nur
// als generischer Text.
describe('tokenErrorField — maps a server error code to the form field it belongs to (Gate a)', () => {
  it('invalid_label → label', () => {
    const e = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_label' })
    expect(tokenErrorField(e)).toBe('label')
  })

  it('unknown_scope → scopes (the load-bearing mapping for Gate a)', () => {
    const e = new ApiError(422, 'validation', 'raw', null, { code: 'unknown_scope' })
    expect(tokenErrorField(e)).toBe('scopes')
  })

  it('invalid_expiry → expires_at', () => {
    const e = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_expiry' })
    expect(tokenErrorField(e)).toBe('expires_at')
  })

  it('an unrelated code (e.g. not_found) maps to no field (generic placement)', () => {
    const e = new ApiError(404, 'not_found', 'raw', null, { code: 'not_found' })
    expect(tokenErrorField(e)).toBeNull()
  })
})

describe('tokensErrorText — keys on the server envelope code, not the HTTP status (Muster settingsErrorText)', () => {
  it('maps invalid_label / unknown_scope / invalid_expiry to distinct, non-empty strings', () => {
    const invalidLabel = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_label' })
    const unknownScope = new ApiError(422, 'validation', 'raw', null, { code: 'unknown_scope' })
    const invalidExpiry = new ApiError(422, 'validation', 'raw', null, { code: 'invalid_expiry' })
    const texts = [invalidLabel, unknownScope, invalidExpiry].map((e) => tokensErrorText(e))
    for (const t of texts) expect(t).toMatch(/\S/)
    expect(new Set(texts).size).toBe(3)
  })

  it('maps not_found (revoke 404) to its own localized string', () => {
    const notFound = new ApiError(404, 'not_found', 'raw', null, { code: 'not_found' })
    expect(tokensErrorText(notFound)).toMatch(/\S/)
  })

  it('falls back to e.message for a code without a dedicated tokens string', () => {
    const e = new ApiError(500, 'server', 'boom', null, { code: 'internal' })
    expect(tokensErrorText(e)).toBe('boom')
  })

  // B2-style invariant (Muster settingsErrorText, §5 B2 sinngemäß auf Tokens übertragen): the function
  // takes only an error — no second (token/secret) argument can carry plaintext into an error string.
  it('takes a single argument (no token/plaintext channel into the error text)', () => {
    expect(tokensErrorText.length).toBe(1)
  })
})

describe('tokenStatusText — every server status value maps to a non-empty localized string', () => {
  it('covers active, revoked and expired distinctly', () => {
    const texts = [tokenStatusText('active'), tokenStatusText('revoked'), tokenStatusText('expired')]
    for (const t of texts) expect(t).toMatch(/\S/)
    expect(new Set(texts).size).toBe(3)
  })
})
