// Wire protocol between the main thread (loader.ts) and the Berry-WASM Worker
// (sim.worker.ts). The VM runs in a Worker per board decision E-A33-2, so a
// runaway script only blocks the Worker thread until the in-WASM 100 ms deadline
// (design/33 §4.2) — the UI thread is never held.

export type SimRequest =
  | { id: number; kind: 'run'; source: string; dev?: string }
  | { id: number; kind: 'compileOnly'; source: string }

/** rc: 0=ok 1=compile-err 2=runtime-err 3=deadline (mirrors sim_run in sim_main.c). */
export interface SimResponse {
  id: number
  rc: number
  error: string
  /** 30000-byte framebuffer; present only for a successful `run`. */
  fb?: Uint8Array
}
