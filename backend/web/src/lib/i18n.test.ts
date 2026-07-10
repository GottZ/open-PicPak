import { expect, test } from 'vitest'
import { localizedDescription } from './i18n'

// localizedDescription resolves a multilingual description map (locale→text) to one string with the
// fallback chain locale → en → de → first key present → '' (design 34-i18n §description, A34.4).

const desc = {
  de: 'Deutsche Beschreibung',
  en: 'English description',
  fr: 'Description française',
}

test('returns the exact locale when present', () => {
  expect(localizedDescription(desc, 'fr')).toBe('Description française')
  expect(localizedDescription(desc, 'de')).toBe('Deutsche Beschreibung')
})

test('falls back to en for a locale with no entry', () => {
  // ru is not in the map → en fallback (not de, not first key).
  expect(localizedDescription(desc, 'ru')).toBe('English description')
})

test('falls back to de when neither the locale nor en is present', () => {
  expect(localizedDescription({ de: 'nur DE' }, 'ja')).toBe('nur DE')
})

test('falls back to the first present key when neither locale, en, nor de exist', () => {
  expect(localizedDescription({ fr: 'seulement FR' }, 'ko')).toBe('seulement FR')
})

test('an empty map yields the empty string', () => {
  expect(localizedDescription({}, 'en')).toBe('')
})

test('a null or undefined map yields the empty string', () => {
  expect(localizedDescription(null, 'en')).toBe('')
  expect(localizedDescription(undefined, 'en')).toBe('')
})
