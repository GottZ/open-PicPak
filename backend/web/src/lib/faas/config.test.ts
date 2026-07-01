import { describe, it, expect } from 'vitest'
import {
  DITHER_OPTIONS,
  buildTriggerConfig,
  parseTriggerFields,
  emptyTriggerFields,
  parseEgress,
  formatEgress,
  webhookURL,
  validCron,
} from './config'

// buildTriggerConfig — only the fields relevant to the type, empties omitted (a minimal row), the shape
// cmd/admin validTriggerConfig accepts. Red: a webhook config carries render cadence, or the default
// dither bloats every row.
describe('buildTriggerConfig', () => {
  it('render → mode/ttl/interval, dither omitted when none', () => {
    const cfg = buildTriggerConfig('render', { ...emptyTriggerFields(), mode: 'sync', ttlS: 120 })
    expect(cfg).toEqual({ mode: 'sync', ttl_s: 120 })
  })
  it('includes a non-default dither, omits none', () => {
    expect(buildTriggerConfig('render', { ...emptyTriggerFields(), dither: 'atkinson' })).toEqual({ dither: 'atkinson' })
    expect(buildTriggerConfig('render', { ...emptyTriggerFields(), dither: 'none' })).toEqual({})
  })
  it('schedule → cron/interval only (no render mode)', () => {
    const cfg = buildTriggerConfig('schedule', { ...emptyTriggerFields(), mode: 'sync', cron: '0 * * * *', intervalS: 900 })
    expect(cfg).toEqual({ cron: '0 * * * *', interval_s: 900 })
    expect(cfg).not.toHaveProperty('mode')
  })
  it('webhook → carries no cadence config', () => {
    expect(buildTriggerConfig('webhook', { ...emptyTriggerFields(), ttlS: 60, cron: '0 0 * * *' })).toEqual({})
  })
  it('roundtrips through parseTriggerFields', () => {
    const built = buildTriggerConfig('render', { ...emptyTriggerFields(), mode: 'prerender', intervalS: 300, dither: 'ordered' })
    const parsed = parseTriggerFields(built)
    expect(parsed.mode).toBe('prerender')
    expect(parsed.intervalS).toBe(300)
    expect(parsed.dither).toBe('ordered')
  })
  it('parseTriggerFields defaults dither to none and clamps junk to 0', () => {
    const f = parseTriggerFields({ ttl_s: -5, interval_s: 'nope' })
    expect(f.dither).toBe('none')
    expect(f.ttlS).toBe(0)
    expect(f.intervalS).toBe(0)
  })
})

// parseEgress — one host per line/comma, trimmed, deduped, order-preserving; empty = no egress.
describe('parseEgress / formatEgress', () => {
  it('splits, trims, dedupes, preserves order', () => {
    expect(parseEgress('a.example:8123\n b.example \n a.example:8123 ,c.example')).toEqual([
      'a.example:8123',
      'b.example',
      'c.example',
    ])
  })
  it('empty editor → empty list (deny-by-default)', () => {
    expect(parseEgress('   \n  ')).toEqual([])
  })
  it('formatEgress round-trips', () => {
    expect(parseEgress(formatEgress(['x.example', 'y.example']))).toEqual(['x.example', 'y.example'])
  })
})

// webhookURL — <base>/faas/hook/<name>, base from /api/config; null (hidden) when unset.
describe('webhookURL', () => {
  it('builds against the ingest base, trimming a trailing slash', () => {
    expect(webhookURL('https://host.example/', 'weather')).toBe('https://host.example/faas/hook/weather')
    expect(webhookURL('https://host.example', 'weather')).toBe('https://host.example/faas/hook/weather')
  })
  it('is null when the base is unset (no hostname baked in)', () => {
    expect(webhookURL('', 'weather')).toBeNull()
    expect(webhookURL('   ', 'weather')).toBeNull()
  })
})

// validCron — best-effort 5-field parse-check; empty allowed (M1 interval fallback).
describe('validCron', () => {
  it('accepts 5 fields and empty, rejects wrong arity', () => {
    expect(validCron('0 * * * *')).toBe(true)
    expect(validCron('  ')).toBe(true)
    expect(validCron('0 * * *')).toBe(false)
    expect(validCron('0 * * * * *')).toBe(false)
  })
})

describe('DITHER_OPTIONS', () => {
  it('marks none golden-exact and offers the three sharp modes', () => {
    expect(DITHER_OPTIONS.map((o) => o.value)).toEqual(['none', 'floyd-steinberg', 'atkinson', 'ordered'])
    expect(DITHER_OPTIONS[0].label).toContain('golden-exact')
  })
})
