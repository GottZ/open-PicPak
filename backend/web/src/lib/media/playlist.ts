// Pure playlist-editor core (design 29 §7 W7) — the reorder reducer, the reorder
// wire body, the dirty predicate and the draft (de)serialisation, all free of DOM
// and sv-router so they load in the vitest node env (the editor component wires
// them). The reorder reducer is the load-bearing piece: the server's PATCH
// /playlists/{id}/items takes a FULL, duplicate-free item_ids order (422 on a gap
// or a repeat), so a local reorder that leaves the position sequence inconsistent
// would desync the editor from what it is about to persist. reorder therefore
// renumbers position to a contiguous 0..n-1 run after every move.

import type { PlaylistItem, OrderMode } from './types'

/**
 * Move the item at index `from` to index `to`, returning a NEW array whose
 * `position` field is renumbered to a contiguous 0..n-1 sequence matching the new
 * order. An out-of-range or same-index move is a no-op (returns the input array).
 *
 * Renumbering is mandatory: the drag surface reads `position` and the persisted
 * order must match the array order exactly, so a move that only reordered the
 * array while leaving stale positions would strand a gap/duplicate in the sequence
 * (the negative probe pins this).
 */
export function reorder<T extends { position: number }>(items: T[], from: number, to: number): T[] {
  const n = items.length
  if (from === to || from < 0 || to < 0 || from >= n || to >= n) return items
  const next = items.slice()
  const [moved] = next.splice(from, 1)
  next.splice(to, 0, moved)
  return next.map((item, i) => (item.position === i ? item : { ...item, position: i }))
}

/** The full, ordered item_ids body for PATCH /api/playlists/{id}/items. */
export function itemIds(items: readonly PlaylistItem[]): number[] {
  return items.map((it) => it.id)
}

/** The editor's mutable state, snapshotted for the baseline and drafted to storage. */
export interface PlaylistDraft {
  name: string
  order_mode: OrderMode
  interval_s: number
  item_ids: number[]
}

/** Snapshot the current editor state into a comparable draft. */
export function draftOf(name: string, order_mode: OrderMode, interval_s: number, items: readonly PlaylistItem[]): PlaylistDraft {
  return { name, order_mode, interval_s, item_ids: itemIds(items) }
}

/**
 * True when the current editor state diverges from the loaded baseline — the
 * signal the dirtyGuard consumes (unsaved meta edit OR a reordered item set). A
 * pure array/scalar compare so the guard and the Save affordance share one truth.
 */
export function playlistDirty(baseline: PlaylistDraft, current: PlaylistDraft): boolean {
  return (
    baseline.name !== current.name ||
    baseline.order_mode !== current.order_mode ||
    baseline.interval_s !== current.interval_s ||
    !sameOrder(baseline.item_ids, current.item_ids)
  )
}

/** Positional array equality (order-sensitive — a reorder must read as a change). */
function sameOrder(a: readonly number[], b: readonly number[]): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

/**
 * Decide what the editor's draft-persistence effect may do — the cross-id guard
 * (lead finding on a2f1c68). During a playlist switch, loadDetail(idB) sets
 * selectedId=B synchronously and only THEN awaits the fetch; Svelte flushes
 * effects inside that await, so an effect keyed on selectedId would read B while
 * baseline/edits still belong to A — persisting A's draft under B's key (a save
 * would rename B to A's name) or clearing B's stored draft before it is ever
 * read. The effect must therefore act only when the LOADED playlist (detail.id)
 * and the SELECTED id agree; in the switch window they differ → 'skip'.
 */
export function draftAction(
  loadedId: number | null,
  selectedId: number | null,
  dirty: boolean,
): 'save' | 'clear' | 'skip' {
  if (loadedId === null || selectedId === null || loadedId !== selectedId) return 'skip'
  return dirty ? 'save' : 'clear'
}

/** Serialise a draft for localStorage (draft.ts stores strings only). */
export function serializeDraft(draft: PlaylistDraft): string {
  return JSON.stringify(draft)
}

/**
 * Parse a persisted draft, returning null for absent/corrupt content (a stale or
 * hand-mangled draft must never crash the editor — it just falls back to the
 * server state, exactly like the FaaS editor).
 */
export function parseDraft(raw: string | null): PlaylistDraft | null {
  if (raw === null) return null
  try {
    const v = JSON.parse(raw) as Partial<PlaylistDraft>
    if (
      typeof v !== 'object' ||
      v === null ||
      typeof v.name !== 'string' ||
      typeof v.order_mode !== 'string' ||
      typeof v.interval_s !== 'number' ||
      !Array.isArray(v.item_ids) ||
      !v.item_ids.every((x) => typeof x === 'number')
    ) {
      return null
    }
    return { name: v.name, order_mode: v.order_mode as OrderMode, interval_s: v.interval_s, item_ids: v.item_ids }
  } catch {
    return null
  }
}
