import { expect, test } from 'vitest'
import { ApiError } from './api'
import { errorText } from './errors'

// In the node test runtime no locale strategy resolves, so Paraglide falls back
// to the baseLocale (de) — the errors.* catalog is exercised in its source
// language. The raw server message stays on ApiError.message, untranslated.

test('errorText maps a known code to its localized catalog entry', () => {
  const e = new ApiError(404, 'not_found', 'row 42 missing from the store')
  expect(errorText(e)).toBe('Nicht gefunden.')
  // The raw server text is preserved separately as the technical detail.
  expect(e.message).toBe('row 42 missing from the store')
})

test('errorText falls back to the generic entry for an unknown code', () => {
  expect(errorText(new ApiError(418, 'http_418', "I'm a teapot"))).toBe('Unbekannter Fehler.')
})
