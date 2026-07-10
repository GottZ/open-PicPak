// Paged<T,C> — the cursor accumulator the library grid needs at target scale
// (design 29 §6/§7 W5). Resource.load() REPLACES data (resource.svelte.ts) so it
// cannot page; this is the second ResourceView implementation, mirroring the
// already-shipped LogViewer buffer+reconcile pattern (routes/logs, Finding 7) as a
// reusable module. It accumulates keyset pages, dedups by a stable key (a repeated
// id across an overlapping page is absorbed exactly once), and survives the two
// races the gate pins: a mid-stream page failure leaves the accumulator intact
// (moreError, status stays 'ready'), and a reload supersedes any in-flight
// load-more so a stale page can never append after the buffer was reset.
//
// It satisfies ResourceView<T[]> (status/data/error/reload) so StateView renders
// its first-load empty / loading / error states through the one D19.13 convention;
// load-more, loading-more and the non-destructive mid-stream error are surfaced
// beside it (the grid renders them under the ready snippet).

import { toApiError, type ApiError } from '../api'
import type { ResourceStatus, ResourceView } from '../resource.svelte'

/** One keyset page: the rows plus the cursor for the NEXT page, or null at the edge. */
export interface CursorPage<T, C> {
  items: T[]
  next: C | null
}

export class Paged<T, C = number> implements ResourceView<T[]> {
  /** First-load lifecycle for StateView. 'error' only when the accumulator is still empty. */
  status = $state<ResourceStatus>('idle')
  /** The accumulated rows across every absorbed page. */
  items = $state<T[]>([])
  /** First-page load failure (empty accumulator) — StateView shows it with a retry. */
  error = $state<ApiError | null>(null)
  /** A load-more request is in flight. */
  loadingMore = $state(false)
  /** A mid-stream (page ≥ 2) failure — NON-destructive: the accumulated rows stay put. */
  moreError = $state<ApiError | null>(null)

  // #exhausted drives hasMore, read in the template → must be reactive. #cursor is
  // internal plumbing (never rendered) so it stays a plain field. #keys is the O(1)
  // dedup set; #seq is the generation guard that aborts a stale in-flight page.
  #exhausted = $state(false)
  #cursor: C | null = null
  #keys = new Set<string | number>()
  #seq = 0
  readonly #fetch: (cursor: C | null) => Promise<CursorPage<T, C>>
  readonly #keyOf: (item: T) => string | number

  constructor(
    fetch: (cursor: C | null) => Promise<CursorPage<T, C>>,
    keyOf: (item: T) => string | number,
  ) {
    this.#fetch = fetch
    this.#keyOf = keyOf
  }

  /** ResourceView: the accumulated rows are the data StateView renders / empty-checks. */
  get data(): T[] {
    return this.items
  }

  /** True while a further keyset page may exist (the last page returned a non-null cursor). */
  get hasMore(): boolean {
    return !this.#exhausted
  }

  /**
   * Reset to the first page. Clears the accumulator + dedup set and bumps the
   * generation so any in-flight load-more from the previous view is discarded when
   * it resolves (the abort the gate pins). Also the W4 uploader refresh hook.
   */
  reload = async (): Promise<void> => {
    const seq = ++this.#seq
    this.status = 'loading'
    this.error = null
    this.moreError = null
    this.loadingMore = false
    this.#cursor = null
    this.#exhausted = false
    this.#keys = new Set()
    this.items = []
    try {
      const page = await this.#fetch(null)
      if (seq !== this.#seq) return // a newer reload superseded this one
      this.#absorb(page)
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
    }
  }

  /**
   * Append the next keyset page. A no-op unless the view is 'ready', has more, and
   * no load-more is already running. A failure is surfaced as moreError WITHOUT
   * touching the accumulator or the 'ready' status (mid-stream resilience). A
   * reload racing this request wins: the generation guard drops the stale page.
   */
  loadMore = async (): Promise<void> => {
    if (this.loadingMore || this.#exhausted || this.status !== 'ready') return
    const seq = this.#seq // capture the generation; do NOT bump (this is not a reset)
    this.loadingMore = true
    this.moreError = null
    try {
      const page = await this.#fetch(this.#cursor)
      if (seq !== this.#seq) return // a reload happened mid-flight → discard this page
      this.#absorb(page)
    } catch (err) {
      if (seq !== this.#seq) return
      this.moreError = toApiError(err) // rows stay intact; status stays 'ready'
    } finally {
      if (seq === this.#seq) this.loadingMore = false
    }
  }

  /**
   * Targeted removal of one accumulated row by key — the delete-flow's
   * list-invalidate (design 29 §6/§7 W6). A successful DELETE drops exactly the
   * one tile WITHOUT a reload(), so the grid never flickers through a loading
   * state or refetches every page (the W6 gate: 200 ⇒ the row vanishes, no full
   * reload). Frees the dedup key too, so a later re-upload of the same id can
   * re-appear. A no-op for an unknown key.
   */
  remove = (key: string | number): void => {
    if (!this.#keys.has(key)) return
    this.#keys.delete(key)
    this.items = this.items.filter((item) => this.#keyOf(item) !== key)
  }

  // Append only rows whose key is new (dedup → "genau einmal" even if pages overlap),
  // then advance the cursor / edge from the page. One array reassignment per page.
  #absorb(page: CursorPage<T, C>): void {
    const added: T[] = []
    for (const item of page.items) {
      const k = this.#keyOf(item)
      if (this.#keys.has(k)) continue
      this.#keys.add(k)
      added.push(item)
    }
    if (added.length > 0) this.items = [...this.items, ...added]
    this.#cursor = page.next
    this.#exhausted = page.next === null
  }
}
