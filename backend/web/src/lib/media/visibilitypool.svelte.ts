// VisibilityPool — the bounded live-node window the target-scale grid needs
// (design 29 §6/§7 W6: "IntersectionObserver … Canvas-Pool/Recycling … nur
// sichtbare Kacheln halten einen [lebenden Knoten]"). The W5 grid renders each
// thumbnail as an <img> against the cacheable /thumbnail route (E-A29-5), so the
// pooled/recycled resource is the mounted <img> node — each holds a decoded
// bitmap in browser RAM, the real cost at 500+ tiles. This class is the pure,
// node-testable heart of that virtualisation: an IntersectionObserver in
// MediaHome reports enter()/exit() as tiles cross the viewport, and the template
// mounts an <img> only for isLive() ids; an off-screen tile keeps a cheap
// aspect-ratio placeholder and no bitmap.
//
// A hard `cap` is a fail-safe CEILING: even if the observer misfires (a burst of
// enters, a dropped exit), the live set can NEVER exceed `cap` — the
// oldest-entered id is recycled (evicted) first. That makes the OOM invariant
// hold independent of observer timing, which is the W6 gate: 500 tiles ⇒ live
// nodes stay bounded, never 500. #order is the live ids in enter() order (LRU);
// it stays in lockstep with #live so #order.length === #live.size always.

const DEFAULT_CAP = 64 // ≈ a tall grid viewport (~30 tiles) + generous overscan.

export class VisibilityPool {
  // The ids whose heavy <img> node is currently mounted. Reactive: the grid reads
  // isLive(id) per tile, so a mount/unmount must re-render exactly that tile.
  #live = $state(new Set<number>())
  // Insertion order for LRU recycling — the oldest-entered id is evicted first
  // when the cap is exceeded.
  #order: number[] = []
  readonly #cap: number

  constructor(cap: number = DEFAULT_CAP) {
    this.#cap = Math.max(1, Math.floor(cap))
  }

  /** A tile entered the viewport (+overscan) → mount its node. Idempotent. When
   *  the cap is exceeded, recycle the oldest live tiles so the ceiling holds. */
  enter(id: number): void {
    if (this.#live.has(id)) return
    const next = new Set(this.#live)
    next.add(id)
    this.#order.push(id)
    while (this.#order.length > this.#cap) {
      const evict = this.#order.shift()
      if (evict === undefined) break
      next.delete(evict)
    }
    this.#live = next
  }

  /** A tile left the viewport (or unmounted) → drop its node, freeing the bitmap. */
  exit(id: number): void {
    if (!this.#live.has(id)) return
    const next = new Set(this.#live)
    next.delete(id)
    this.#live = next
    const at = this.#order.indexOf(id)
    if (at !== -1) this.#order.splice(at, 1)
  }

  /** Does this tile currently hold a live <img> node? Drives the {#if} per tile. */
  isLive(id: number): boolean {
    return this.#live.has(id)
  }

  /** Live node count — the OOM gate asserts this stays ≤ cap at 500 tiles. */
  get size(): number {
    return this.#live.size
  }

  /** The hard live-node ceiling (fail-safe against observer misfire). */
  get cap(): number {
    return this.#cap
  }
}
