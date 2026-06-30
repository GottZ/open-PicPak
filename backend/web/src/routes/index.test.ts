// F4 — route-table gates (design 19 §6): the areas exist, every route stays lazy,
// none collides with a path cmd/admin registers on the server (a mistyped SPA
// route must not shadow /api or /healthz), the nav metadata stays in sync with
// the table, and `/` canonicalizes to /fleet.
//
// The membership check is a SUPERSET (arrayContaining): the feature docs replace
// placeholder slots but the key set is stable, so this stays additive-safe.

import { describe, expect, it } from 'vitest'
import { AREAS, RESERVED_SERVER_PREFIXES, areaRoutes, entryRedirect } from './index'

const BASE_AREAS = [
  '*',
  '/berry',
  '/fleet',
  '/functions',
  '/logs',
  '/onboard',
  '/ota',
  '/settings',
] as const

describe('areaRoutes', () => {
  it('registers at least the base areas and the catch-all', () => {
    expect(Object.keys(areaRoutes)).toEqual(expect.arrayContaining([...BASE_AREAS]))
  })

  it('keeps every area lazy (separate chunk per area)', () => {
    for (const loader of Object.values(areaRoutes)) {
      expect(typeof loader).toBe('function')
    }
  })

  it('claims no path inside the reserved server namespace', () => {
    const paths = Object.keys(areaRoutes).filter((p) => p.startsWith('/'))
    for (const path of paths) {
      for (const reserved of RESERVED_SERVER_PREFIXES) {
        expect(path === reserved || path.startsWith(`${reserved}/`)).toBe(false)
      }
    }
  })

  it('keeps the AREAS nav metadata in sync with the route table (no drift)', () => {
    const routeKeys = new Set(Object.keys(areaRoutes))
    for (const area of AREAS) {
      expect(routeKeys.has(area.path)).toBe(true)
    }
  })
})

describe('entryRedirect', () => {
  it('canonicalizes / to /fleet', () => {
    expect(entryRedirect('/')).toBe('/fleet')
  })

  it('leaves every real route alone', () => {
    for (const path of ['/fleet', '/ota', '/logs', '/berry', '/functions', '/onboard', '/settings', '/nope', '']) {
      expect(entryRedirect(path)).toBeNull()
    }
  })
})
