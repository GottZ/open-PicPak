// Main-thread handle to the Berry-WASM simulator Worker (design/33 §4.2, E-A33-2).
// The Worker and the ~80 KB .wasm.br it loads are OFF the initial bundle: callers
// import this module dynamically (idle after editor mount / on simulator open,
// design/33 §4.4), and Vite emits the Worker as its own chunk from the
// `new Worker(new URL(...))` form below. Without WASM the editor degrades to the
// static lint.ts path (fail-open for diagnostics; on-device stays authoritative).
import type { SimRequest, SimResponse } from './protocol'

// Distribute Omit over each union member (naked type param), so the run-only
// `dev` survives; a bare Omit<SimRequest,'id'> collapses to the common keys.
type DistributiveOmit<T, K extends PropertyKey> = T extends unknown ? Omit<T, K> : never
type SimRequestBody = DistributiveOmit<SimRequest, 'id'>

export interface SimResult {
  /** 0=ok 1=compile-err 2=runtime-err 3=deadline (-1=worker/load failure). */
  rc: number
  error: string
  /** 30000-byte framebuffer; present only for a successful run(). */
  fb?: Uint8Array
}

export class BerrySim {
  #worker: Worker
  #seq = 0
  #pending = new Map<number, (r: SimResponse) => void>()

  constructor() {
    this.#worker = new Worker(new URL('./sim.worker.ts', import.meta.url), { type: 'module' })
    this.#worker.onmessage = (e: MessageEvent<SimResponse>) => {
      const resolve = this.#pending.get(e.data.id)
      if (resolve) {
        this.#pending.delete(e.data.id)
        resolve(e.data)
      }
    }
  }

  #send(req: SimRequestBody): Promise<SimResult> {
    const id = ++this.#seq
    return new Promise<SimResult>((resolve) => {
      this.#pending.set(id, (r) => resolve({ rc: r.rc, error: r.error, fb: r.fb }))
      this.#worker.postMessage({ ...req, id } as SimRequest)
    })
  }

  /** Run a Berry script; returns rc + the 30000-byte framebuffer on success. */
  run(source: string, dev?: string): Promise<SimResult> {
    return this.#send({ kind: 'run', source, dev })
  }

  /** Syntax-check only (third linter() source, design/33 §4.4) — never executes. */
  compileOnly(source: string): Promise<SimResult> {
    return this.#send({ kind: 'compileOnly', source })
  }

  dispose() {
    this.#worker.terminate()
    this.#pending.clear()
  }
}
