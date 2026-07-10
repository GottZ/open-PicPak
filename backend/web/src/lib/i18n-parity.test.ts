import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, test } from 'vitest'

// Catalog-parity gate (A34.3). Paraglide's message-format plugin silently falls
// back a missing key to the baseLocale at runtime instead of erroring at compile
// time, so `bun run check` alone does NOT catch a key dropped from a non-base
// locale. This test is the real red gate: every locale in settings.json must be
// key-for-key identical to the de baseLocale, and every placeholder token
// ({name}, {version}, …) must survive translation byte-identical — a renamed or
// dropped param would break `m['key']({ … })` callsites. baseLocale + locales are
// read from project.inlang so the list can never drift out of sync with the app.

const WEB_ROOT = process.cwd() // vitest runs from backend/web
const MSG_DIR = join(WEB_ROOT, 'messages')
const settings = JSON.parse(
  readFileSync(join(WEB_ROOT, 'project.inlang', 'settings.json'), 'utf8'),
) as { baseLocale: string; locales: string[] }

const PARAM = /\{[a-zA-Z0-9_]+\}/g

type Catalog = Record<string, string>
function load(locale: string): Catalog {
  return JSON.parse(readFileSync(join(MSG_DIR, `${locale}.json`), 'utf8')) as Catalog
}
function messageKeys(cat: Catalog): string[] {
  return Object.keys(cat).filter((k) => k !== '$schema')
}
function params(value: string): string[] {
  return (value.match(PARAM) ?? []).slice().sort()
}

const base = load(settings.baseLocale)
const baseKeys = messageKeys(base).sort()

test('settings.json carries the full 9-locale set', () => {
  expect(settings.locales).toEqual(['de', 'en', 'fr', 'it', 'es', 'ru', 'zh', 'ko', 'ja'])
})

describe.each(settings.locales)('locale %s', (locale) => {
  const cat = load(locale)

  test('has the exact same message keys as the baseLocale', () => {
    expect(messageKeys(cat).sort()).toEqual(baseKeys)
  })

  test('$schema matches the baseLocale', () => {
    expect(cat['$schema']).toBe(base['$schema'])
  })

  test('every message preserves the baseLocale placeholder set byte-identically', () => {
    const mismatches = baseKeys
      .filter((k) => cat[k] !== undefined)
      .map((k) => ({ k, want: params(base[k]), got: params(cat[k]) }))
      .filter(({ want, got }) => JSON.stringify(want) !== JSON.stringify(got))
    expect(mismatches).toEqual([])
  })
})
