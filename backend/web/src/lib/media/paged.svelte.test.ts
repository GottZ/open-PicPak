// W5 gate (design 29 §6/§7 W5): the cursor accumulator must (a) accumulate a page
// sequence (cursor→cursor→null) FULLY and exactly once, (b) reload → empty + reload
// from the first page, (c) survive a mid-stream failure with the accumulated rows
// intact + the error surfaced, and (d) discard a stale in-flight load-more after a
// reload. Each has a red belt: the dedup test fails without dedup, the abort test
// fails without the generation guard. Pure node — the Paged runes compile in node
// (vite.config.ts test comment), no DOM.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../api'
import { Paged, type CursorPage } from './paged.svelte'

interface Row {
  id: number
}

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void
  let reject!: (e: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

const ids = (p: Paged<Row>): number[] => p.items.map((r) => r.id)

// A keyset fixture keyed by the cursor value (null = first page). Mirrors the real
// GET /api/images ?after=<id> contract: each page carries the next cursor, null at
// the edge.
function pagesByCursor(map: Record<string, CursorPage<Row, number>>) {
  return async (cursor: number | null): Promise<CursorPage<Row, number>> => {
    const page = map[cursor === null ? 'null' : String(cursor)]
    if (!page) throw new Error(`no fixture page for cursor ${cursor}`)
    return page
  }
}

describe('Paged — cursor accumulator', () => {
  const three: Record<string, CursorPage<Row, number>> = {
    null: { items: [{ id: 1 }, { id: 2 }], next: 2 },
    '2': { items: [{ id: 3 }, { id: 4 }], next: 4 },
    '4': { items: [{ id: 5 }], next: null },
  }

  it('accumulates a full cursor sequence (cursor→cursor→null) exactly once', async () => {
    const p = new Paged<Row>(pagesByCursor(three), (r) => r.id)
    expect(p.status).toBe('idle')

    await p.reload()
    expect(ids(p)).toEqual([1, 2])
    expect(p.status).toBe('ready')
    expect(p.hasMore).toBe(true)

    await p.loadMore()
    expect(ids(p)).toEqual([1, 2, 3, 4])
    expect(p.hasMore).toBe(true)

    await p.loadMore()
    expect(ids(p)).toEqual([1, 2, 3, 4, 5])
    expect(p.hasMore).toBe(false)

    // Exhausted → load-more is a no-op (no fetch, no error).
    await p.loadMore()
    expect(ids(p)).toEqual([1, 2, 3, 4, 5])
    expect(p.moreError).toBeNull()
  })

  it('dedups an overlapping id across pages (accumulates exactly once) — red without dedup', async () => {
    // Page 2 repeats id 2 from page 1. A replace/no-dedup accumulator yields [1,2,2,3].
    const overlap = pagesByCursor({
      null: { items: [{ id: 1 }, { id: 2 }], next: 2 },
      '2': { items: [{ id: 2 }, { id: 3 }], next: null },
    })
    const p = new Paged<Row>(overlap, (r) => r.id)
    await p.reload()
    await p.loadMore()
    expect(ids(p)).toEqual([1, 2, 3])
  })

  it('reload empties the accumulator and reloads from the first page', async () => {
    const p = new Paged<Row>(pagesByCursor(three), (r) => r.id)
    await p.reload()
    await p.loadMore()
    expect(ids(p)).toEqual([1, 2, 3, 4])

    await p.reload()
    expect(ids(p)).toEqual([1, 2]) // reset to the first page, not appended
    expect(p.hasMore).toBe(true)
    expect(p.moreError).toBeNull()
  })

  it('keeps the accumulated rows intact when a mid-stream page fails', async () => {
    const boom = new ApiError(500, 'server', 'boom')
    const fetch = async (cursor: number | null): Promise<CursorPage<Row, number>> => {
      if (cursor === null) return { items: [{ id: 1 }, { id: 2 }], next: 2 }
      throw boom
    }
    const p = new Paged<Row>(fetch, (r) => r.id)
    await p.reload()
    expect(ids(p)).toEqual([1, 2])

    await p.loadMore()
    expect(ids(p)).toEqual([1, 2]) // accumulator untouched by the failure
    expect(p.status).toBe('ready') // NOT 'error' — the grid stays rendered
    expect(p.moreError?.message).toBe('boom')
    expect(p.loadingMore).toBe(false)
    expect(p.hasMore).toBe(true) // cursor not advanced → a retry is possible
  })

  it('surfaces a first-page failure as the error status (empty accumulator)', async () => {
    const fetch = async (): Promise<CursorPage<Row, number>> => {
      throw new ApiError(503, 'server', 'down')
    }
    const p = new Paged<Row>(fetch, (r) => r.id)
    await p.reload()
    expect(p.status).toBe('error')
    expect(p.error?.message).toBe('down')
    expect(ids(p)).toEqual([])
  })

  it('discards a stale in-flight load-more after a reload — red without the generation guard', async () => {
    const gate = deferred<CursorPage<Row, number>>()
    const fetch = async (cursor: number | null): Promise<CursorPage<Row, number>> => {
      if (cursor === null) return { items: [{ id: 1 }, { id: 2 }], next: 2 }
      return gate.promise // the (only) load-more hangs until we release it
    }
    const p = new Paged<Row>(fetch, (r) => r.id)
    await p.reload() // [1,2], cursor → 2

    const stale = p.loadMore() // fetch(2) → pending
    await p.reload() // supersede: bumps the generation, resets to [1,2]
    gate.resolve({ items: [{ id: 99 }], next: null }) // the stale page finally resolves
    await stale

    expect(ids(p)).toEqual([1, 2]) // the stale {id:99} was discarded, not appended
    expect(p.hasMore).toBe(true)
  })

  it('remove() drops one row + its dedup key without a reload (W6 list-invalidate)', async () => {
    const fetch = async (): Promise<CursorPage<Row, number>> => ({
      items: [{ id: 1 }, { id: 2 }, { id: 3 }],
      next: null,
    })
    const p = new Paged<Row>(fetch, (r) => r.id)
    await p.reload()

    p.remove(2)
    expect(ids(p)).toEqual([1, 3]) // exactly the deleted tile is gone
    expect(p.status).toBe('ready') // no reload → no loading flicker
    p.remove(999) // unknown key → no-op
    expect(ids(p)).toEqual([1, 3])

    // The dedup key was freed: a re-uploaded id 2 re-appears on the next absorb.
    p.remove(1)
    expect(ids(p)).toEqual([3])
  })
})
