// W6 OOM gate (design 29 §6/§7 W6): at 500 tiles only the visibility window may
// hold live <img>/bitmap nodes — the grid must never mount all 500. The pool is
// the pure heart of that virtualisation; the .svelte grid wiring (an
// IntersectionObserver) is svelte-check-only (no DOM tests, vite.config.ts test
// comment), so this pins the invariant on the logic. Each case carries a red
// belt: a naive un-virtualised grid (every enter, no exit) would hold 500 nodes —
// the recycling ceiling is what makes that impossible.

import { describe, expect, it } from 'vitest'
import { VisibilityPool } from './visibilitypool.svelte'

describe('VisibilityPool — bounded live-node window', () => {
  it('tracks a sliding viewport window over 500 tiles, never all 500', () => {
    const pool = new VisibilityPool(64)
    const WINDOW = 30 // tiles on screen + overscan at any scroll position
    let peak = 0
    for (let top = 0; top + WINDOW <= 500; top++) {
      pool.enter(top + WINDOW - 1) // a new tile scrolls in at the bottom
      if (top > 0) pool.exit(top - 1) // the previous top tile scrolls out
      peak = Math.max(peak, pool.size)
    }
    expect(peak).toBeLessThanOrEqual(WINDOW + 1)
    expect(peak).toBeLessThan(500) // red belt: an un-virtualised grid holds 500
  })

  it('caps live nodes even when 500 enter with no exit (recycling ceiling)', () => {
    // The virtualisation-disabled pathology: the observer floods enters and never
    // fires an exit. An unbounded Set would grow to 500 live bitmaps (OOM). The
    // pool recycles the oldest, so the live set stays at the hard cap.
    const pool = new VisibilityPool(64)
    for (let id = 0; id < 500; id++) pool.enter(id)
    expect(pool.size).toBe(64) // bounded, NOT 500
    expect(pool.isLive(499)).toBe(true) // newest cap-many stay live
    expect(pool.isLive(500 - 64)).toBe(true) // id 436 — the oldest still-live tile
    expect(pool.isLive(500 - 64 - 1)).toBe(false) // id 435 — recycled
    expect(pool.isLive(0)).toBe(false) // the first tile was recycled long ago
  })

  it('enter/exit is idempotent — no double-count, no underflow', () => {
    const pool = new VisibilityPool(64)
    pool.enter(7)
    pool.enter(7)
    expect(pool.size).toBe(1)
    pool.exit(7)
    pool.exit(7)
    expect(pool.size).toBe(0)
    expect(pool.isLive(7)).toBe(false)
  })

  it('a re-enter after exit keeps the pool consistent with #order', () => {
    const pool = new VisibilityPool(2)
    pool.enter(1)
    pool.enter(2)
    pool.exit(1)
    pool.enter(3) // fills back to cap; 2 is now the oldest live
    pool.enter(4) // exceeds cap → recycle 2 (oldest), keep 3 + 4
    expect(pool.size).toBe(2)
    expect(pool.isLive(2)).toBe(false)
    expect(pool.isLive(3)).toBe(true)
    expect(pool.isLive(4)).toBe(true)
  })
})
