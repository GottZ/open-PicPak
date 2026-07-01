import { describe, it, expect } from 'vitest'
import {
  CAP_ENTRIES,
  CTX_ENTRIES,
  RETURN_ENTRIES,
  TEMPLATE,
  secretEntries,
  completionOptions,
} from './catalog'

const labels = (bound: string[]) => completionOptions(bound).map((e) => e.label)

// T7 — the completion offers `cap.secrets.<name>` ONLY for the function's BOUND names (D25.7). Red: the
// editor completes any/every secret name → it implies access the worker will not grant.
describe('completionOptions — bound-secret gating (T7)', () => {
  it('offers a bound secret and NOT an unbound one', () => {
    const l = labels(['ha_token'])
    expect(l).toContain('cap.secrets.ha_token')
    expect(l).not.toContain('cap.secrets.other')
  })
  it('offers a name only once it is bound', () => {
    expect(labels([])).not.toContain('cap.secrets.other')
    expect(labels(['other'])).toContain('cap.secrets.other')
  })
  it('emits no cap.secrets.<name> when nothing is bound', () => {
    expect(labels([]).some((x) => x.startsWith('cap.secrets.'))).toBe(false)
  })
  it('secretEntries maps each bound name exactly once', () => {
    expect(secretEntries(['a', 'b']).map((e) => e.label)).toEqual(['cap.secrets.a', 'cap.secrets.b'])
  })
})

// The curated static surface is always present (documentation of the D25.14 scope).
describe('completionOptions — static curated scope', () => {
  it('always offers the cap.* capabilities and ctx roots', () => {
    const l = labels([])
    for (const req of ['cap', 'ctx', 'cap.fetch', 'cap.sharp', 'cap.log', 'ctx.serial', 'ctx.payload']) {
      expect(l, req).toContain(req)
    }
  })
  it('CAP/CTX/RETURN entries carry a signature + doc', () => {
    for (const e of [...CAP_ENTRIES, ...CTX_ENTRIES, ...RETURN_ENTRIES]) {
      expect(e.detail.length, e.label).toBeGreaterThan(0)
      expect(e.info.length, e.label).toBeGreaterThan(0)
    }
  })
})

// The template scaffolds exactly the one D25.14 signature the worker invokes.
describe('TEMPLATE', () => {
  it('is the pinned export-default async (ctx, cap) signature returning { image }', () => {
    expect(TEMPLATE).toContain('export default async (ctx, cap) =>')
    expect(TEMPLATE).toContain('return { image')
  })
  it('is air-gap clean (no real host / serial / secret)', () => {
    expect(/gottz|homecore|grogru|\d{1,3}(\.\d{1,3}){3}|\.local/.test(TEMPLATE)).toBe(false)
  })
})
