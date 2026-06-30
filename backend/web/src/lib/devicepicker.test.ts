// D19.12 — device-picker core: staleness, typeahead filter, recency sort.

import { describe, expect, it } from 'vitest'
import { filterDevices, isStale, lastSeenMs, sortByRecency, staleDays, STALE_AFTER_MS } from './devicepicker'
import type { Device } from './api/types'

const NOW = 1_000_000_000_000 // fixed "now" in ms

function dev(p: Partial<Device> & { serial: string }): Device {
  return { label: null, channel: 'stable', last_seen: null, bonded: false, ...p }
}

const recent = dev({ serial: 'D2RCNT1', label: 'Recent', last_seen: new Date(NOW - 1000).toISOString() })
const old = dev({ serial: 'D2OLD02', label: 'Old', last_seen: new Date(NOW - STALE_AFTER_MS - 1000).toISOString() })
const never = dev({ serial: 'D2NEVR3', label: 'Never', last_seen: null })

describe('lastSeenMs', () => {
  it('parses an ISO timestamp, null for absent/invalid', () => {
    expect(lastSeenMs(recent)).toBe(NOW - 1000)
    expect(lastSeenMs(never)).toBeNull()
    expect(lastSeenMs(dev({ serial: 'X', last_seen: 'not-a-date' }))).toBeNull()
  })
})

describe('isStale / staleDays', () => {
  it('a recently-seen device is fresh', () => {
    expect(isStale(recent, NOW)).toBe(false)
  })
  it('a long-silent device is stale', () => {
    expect(isStale(old, NOW)).toBe(true)
    expect(staleDays(old, NOW)).toBeGreaterThanOrEqual(1)
  })
  it('a never-seen device is stale with null days', () => {
    expect(isStale(never, NOW)).toBe(true)
    expect(staleDays(never, NOW)).toBeNull()
  })
})

describe('filterDevices', () => {
  const all = [recent, old, never]
  it('returns all on an empty query', () => {
    expect(filterDevices(all, '   ')).toHaveLength(3)
  })
  it('matches label case-insensitively', () => {
    expect(filterDevices(all, 'recent')).toEqual([recent])
  })
  it('matches serial', () => {
    expect(filterDevices(all, 'old02')).toEqual([old])
  })
})

describe('sortByRecency', () => {
  it('most-recent first, never-seen last, serial tiebreak', () => {
    const sorted = sortByRecency([never, old, recent])
    expect(sorted.map((d) => d.serial)).toEqual(['D2RCNT1', 'D2OLD02', 'D2NEVR3'])
  })
  it('does not mutate the input', () => {
    const input = [never, recent]
    sortByRecency(input)
    expect(input.map((d) => d.serial)).toEqual(['D2NEVR3', 'D2RCNT1'])
  })
})
