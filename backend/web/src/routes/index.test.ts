// F4 — route-table gates (design 19 §6): the areas exist, every route stays lazy,
// none collides with a path cmd/admin registers on the server (a mistyped SPA
// route must not shadow /api or /healthz), the nav metadata stays in sync with
// the table, and `/` canonicalizes to /fleet.
//
// The membership check is a SUPERSET (arrayContaining): the feature docs replace
// placeholder slots but the key set is stable, so this stays additive-safe.

import { describe, expect, it } from 'vitest'
import {
  AREAS,
  RESERVED_SERVER_PREFIXES,
  advancedAreas,
  areaRoutes,
  coreAreas,
  entryRedirect,
} from './index'

const BASE_AREAS = [
  '*',
  '/berry',
  '/fleet',
  '/functions',
  '/logs',
  '/media',
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

// A33.6 probe (a) — the tier field is mandatory on every area (progressive
// disclosure, design 33 §4.6 E-A33-5). An area missing tier fails the TS type
// AND this namespace check; coreAreas/advancedAreas must partition AREAS exactly.
describe('area tiers (progressive disclosure)', () => {
  it('assigns every area a valid tier', () => {
    for (const area of AREAS) {
      expect(area.tier === 'core' || area.tier === 'advanced').toBe(true)
    }
  })

  it('pins core = /gallery, /media, /fleet', () => {
    expect(coreAreas().map((a) => a.path).sort()).toEqual(['/fleet', '/gallery', '/media'])
  })

  it('partitions AREAS into core + advanced with no overlap or loss', () => {
    const core = coreAreas()
    const advanced = advancedAreas()
    expect(core.length + advanced.length).toBe(AREAS.length)
    expect(core.every((a) => a.tier === 'core')).toBe(true)
    expect(advanced.every((a) => a.tier === 'advanced')).toBe(true)
  })

  // A33.6 probe (b) — a deep link to an advanced area resolves regardless of the
  // nav disclosure state: routing is flat (areaRoutes), tier only groups the nav.
  it('routes every advanced area (deep link independent of the disclosure)', () => {
    const routeKeys = new Set(Object.keys(areaRoutes))
    for (const area of advancedAreas()) {
      expect(routeKeys.has(area.path)).toBe(true)
      expect(entryRedirect(area.path)).toBeNull()
    }
    // /functions is an advanced area — its deep link never redirects.
    expect(advancedAreas().some((a) => a.path === '/functions')).toBe(true)
    expect(entryRedirect('/functions')).toBeNull()
  })
})

describe('entryRedirect', () => {
  // A33.6 probe (c) — order pin: `/` lands on /gallery, and that target must be a
  // real area in areaRoutes (redirect bound to the existence of the area).
  it('canonicalizes / to /gallery', () => {
    expect(entryRedirect('/')).toBe('/gallery')
  })

  it('redirects only to an area that exists in areaRoutes', () => {
    const target = entryRedirect('/')
    expect(target).not.toBeNull()
    expect(Object.keys(areaRoutes)).toContain(target)
  })

  it('leaves every real route alone', () => {
    for (const path of ['/gallery', '/fleet', '/ota', '/logs', '/berry', '/functions', '/media', '/onboard', '/settings', '/nope', '']) {
      expect(entryRedirect(path)).toBeNull()
    }
  })
})
