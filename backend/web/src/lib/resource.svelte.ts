// Resource<T> (design 19 §4 / D19.13): the one loading convention for every data
// path — components render the three-stage {#if} cascade off status instead of
// scattering ad-hoc awaits through templates. StateView.svelte (W5) wraps this.

import { toApiError, type ApiError } from './api'

export type ResourceStatus = 'idle' | 'loading' | 'ready' | 'error'

/**
 * The three-stage contract StateView renders off (status / data / error / reload).
 * Resource is the single-shot implementation; the paged accumulator
 * (lib/media/paged.svelte.ts, design 29 §6/§7 W5) is a second implementation, so
 * StateView takes this interface — not the concrete Resource — and both feed the
 * one empty / loading / error convention (D19.13).
 */
export interface ResourceView<T> {
  readonly status: ResourceStatus
  readonly data: T | null
  readonly error: ApiError | null
  reload: () => Promise<void> | void
}

export class Resource<T> implements ResourceView<T> {
  status = $state<ResourceStatus>('idle')
  data = $state<T | null>(null)
  error = $state<ApiError | null>(null)

  #fetcher: () => Promise<T>
  #seq = 0

  constructor(fetcher: () => Promise<T>) {
    this.#fetcher = fetcher
  }

  /** Load (or reload). Stale in-flight loads are superseded, never applied. */
  async load(): Promise<void> {
    const seq = ++this.#seq
    this.status = 'loading'
    this.error = null
    try {
      const data = await this.#fetcher()
      if (seq !== this.#seq) return
      this.data = data
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()
}
