// W7 gate (design 29 §7): the pure playlist-editor core. The load-bearing probe is
// the reorder reducer — the server's PATCH /items rejects a gap/duplicate order
// with a 422, so a move that leaves `position` inconsistent with the array order
// would desync the editor from what it persists. The RED belt: a reducer that
// splices the array but forgets to renumber `position` leaves a stale/duplicate
// sequence, and `expect positions to be a contiguous 0..n-1 run` fails. Pure node.

import { describe, expect, it } from 'vitest'
import {
  reorder,
  itemIds,
  draftOf,
  playlistDirty,
  serializeDraft,
  parseDraft,
  draftAction,
  type PlaylistDraft,
} from './playlist'
import type { PlaylistItem } from './types'

function item(id: number, position: number): PlaylistItem {
  return { id, playlist_id: 1, image_id: id * 10, position, fit: 'cover', dither: 'none' }
}

// A playlist of 4 items at contiguous positions 0..3.
function fixture(): PlaylistItem[] {
  return [item(11, 0), item(12, 1), item(13, 2), item(14, 3)]
}

/** The invariant the server enforces: positions are exactly 0..n-1, in array order. */
function positions(items: PlaylistItem[]): number[] {
  return items.map((it) => it.position)
}

describe('reorder', () => {
  it('moves an item down and renumbers positions to a contiguous 0..n-1 run', () => {
    // move index 0 (id 11) to index 2 → order becomes 12,13,11,14
    const out = reorder(fixture(), 0, 2)
    expect(out.map((it) => it.id)).toEqual([12, 13, 11, 14])
    // RED belt: without renumbering, positions would be [1,2,0,3] — a scrambled,
    // non-contiguous sequence. The reducer must renumber to 0..n-1 in array order.
    expect(positions(out)).toEqual([0, 1, 2, 3])
  })

  it('moves an item up and renumbers positions', () => {
    // move index 3 (id 14) to index 1 → order becomes 11,14,12,13
    const out = reorder(fixture(), 3, 1)
    expect(out.map((it) => it.id)).toEqual([11, 14, 12, 13])
    expect(positions(out)).toEqual([0, 1, 2, 3])
  })

  it('produces no duplicate positions after a move', () => {
    const out = reorder(fixture(), 1, 3)
    expect(new Set(positions(out)).size).toBe(out.length)
  })

  it('is a no-op for a same-index or out-of-range move (returns the input array)', () => {
    const src = fixture()
    expect(reorder(src, 2, 2)).toBe(src)
    expect(reorder(src, -1, 2)).toBe(src)
    expect(reorder(src, 0, 9)).toBe(src)
  })

  it('does not mutate the input array', () => {
    const src = fixture()
    reorder(src, 0, 3)
    expect(src.map((it) => it.id)).toEqual([11, 12, 13, 14])
    expect(positions(src)).toEqual([0, 1, 2, 3])
  })
})

describe('itemIds / draftOf', () => {
  it('extracts the ordered id list for the reorder body', () => {
    expect(itemIds(fixture())).toEqual([11, 12, 13, 14])
  })

  it('snapshots the editor state into a comparable draft', () => {
    expect(draftOf('loop', 'shuffle', 300, fixture())).toEqual({
      name: 'loop',
      order_mode: 'shuffle',
      interval_s: 300,
      item_ids: [11, 12, 13, 14],
    })
  })
})

describe('playlistDirty', () => {
  const baseline: PlaylistDraft = { name: 'loop', order_mode: 'sequential', interval_s: 900, item_ids: [11, 12, 13] }

  it('is clean when nothing changed', () => {
    expect(playlistDirty(baseline, { ...baseline, item_ids: [11, 12, 13] })).toBe(false)
  })

  it('is dirty on a meta edit (name / order_mode / interval_s)', () => {
    expect(playlistDirty(baseline, { ...baseline, name: 'renamed' })).toBe(true)
    expect(playlistDirty(baseline, { ...baseline, order_mode: 'shuffle' })).toBe(true)
    expect(playlistDirty(baseline, { ...baseline, interval_s: 60 })).toBe(true)
  })

  it('is dirty on a reordered item set (order-sensitive, same members)', () => {
    expect(playlistDirty(baseline, { ...baseline, item_ids: [13, 12, 11] })).toBe(true)
  })

  it('is dirty on an added or removed item', () => {
    expect(playlistDirty(baseline, { ...baseline, item_ids: [11, 12, 13, 14] })).toBe(true)
    expect(playlistDirty(baseline, { ...baseline, item_ids: [11, 12] })).toBe(true)
  })
})

describe('draftAction — the cross-id switch guard (lead finding on a2f1c68)', () => {
  // The switch window: loadDetail(B) has set selectedId=B synchronously, but the
  // await has not resolved — detail/baseline/edits still belong to A. An effect
  // acting here would persist A's draft under B's key (a later Save renames B to
  // A's name — the corruption path) or clear B's stored draft before it is read.
  it("skips while the loaded playlist and the selection disagree (A loaded, B selected)", () => {
    expect(draftAction(1, 2, true)).toBe('skip') // A dirty → must NOT save under B's key
    expect(draftAction(1, 2, false)).toBe('skip') // A clean → must NOT clear B's draft
  })

  it('skips while nothing is loaded or selected (boot, post-delete, error path detail=null)', () => {
    expect(draftAction(null, 2, true)).toBe('skip') // fetch in flight / failed: detail=null
    expect(draftAction(1, null, false)).toBe('skip')
    expect(draftAction(null, null, false)).toBe('skip')
  })

  it('saves a dirty and clears a clean draft once loaded and selected agree', () => {
    expect(draftAction(2, 2, true)).toBe('save')
    expect(draftAction(2, 2, false)).toBe('clear')
  })
})

describe('draft persistence round-trip', () => {
  it('serialises and parses a draft', () => {
    const d = draftOf('loop', 'shuffle', 120, fixture())
    expect(parseDraft(serializeDraft(d))).toEqual(d)
  })

  it('returns null for absent or corrupt draft content', () => {
    expect(parseDraft(null)).toBeNull()
    expect(parseDraft('{not json')).toBeNull()
    expect(parseDraft('{"name":"x"}')).toBeNull() // missing fields
    expect(parseDraft('{"name":"x","order_mode":"shuffle","interval_s":10,"item_ids":["a"]}')).toBeNull()
  })
})
