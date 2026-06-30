// Shared device-picker logic (design 19 D19.12, K13). Every "pick a device"
// surface — the A21 log filter, the A23 Berry target, the A25 bind UI — consumes
// the one DevicePicker.svelte (fed by GET /api/devices), not a per-surface
// window query. This module is its pure core: filter, recency sort, staleness.
// Extracted so the matching/sort/stale rules are node-testable.

import type { Device } from './api/types'

const DAY_MS = 24 * 60 * 60 * 1000

/** A device not seen within this window reads as stale — an enqueue to it "will
 *  sit pending" until it next polls (the inline warning on a stale target). */
export const STALE_AFTER_MS = DAY_MS

/** last_seen as epoch ms, or null when absent / unparseable. */
export function lastSeenMs(d: Device): number | null {
  if (!d.last_seen) return null
  const t = Date.parse(d.last_seen)
  return Number.isNaN(t) ? null : t
}

/** A never-seen or long-silent device is stale (blind-enqueue guard, D19.12). */
export function isStale(d: Device, nowMs: number): boolean {
  const t = lastSeenMs(d)
  return t === null || nowMs - t > STALE_AFTER_MS
}

/** Whole days since last_seen, or null when never seen (for the warning copy). */
export function staleDays(d: Device, nowMs: number): number | null {
  const t = lastSeenMs(d)
  if (t === null) return null
  return Math.floor((nowMs - t) / DAY_MS)
}

/** Case-insensitive typeahead over label OR serial. Empty query → all. */
export function filterDevices(devices: Device[], query: string): Device[] {
  const q = query.trim().toLowerCase()
  if (!q) return devices
  return devices.filter(
    (d) => (d.label ?? '').toLowerCase().includes(q) || d.serial.toLowerCase().includes(q),
  )
}

/** Recency sort: most-recently-seen first, never-seen last, serial as tiebreak. */
export function sortByRecency(devices: Device[]): Device[] {
  return [...devices].sort((a, b) => {
    const ta = lastSeenMs(a)
    const tb = lastSeenMs(b)
    if (ta === null && tb === null) return a.serial.localeCompare(b.serial)
    if (ta === null) return 1
    if (tb === null) return -1
    if (tb !== ta) return tb - ta
    return a.serial.localeCompare(b.serial)
  })
}
