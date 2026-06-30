// One toast/notification region for the whole shell (design 19 D19.11, K13). A
// single store + a shell region (Toaster.svelte, owned by App.svelte) renders
// success|info|warn|error toasts. Contract: notify.error(apiError) auto-surfaces
// the server `code` + `X-Request-ID` (copyable); notify.{success,info,warn}(msg)
// for the rest. Every feature mutation (forms in 18/20, editors in 23/25,
// rollout in 20, onboarding in 26) calls notify.* — they do NOT invent
// per-surface banners. One feedback surface, one shape.

import { ApiError } from './api'

export type ToastKind = 'success' | 'info' | 'warn' | 'error'

export interface Toast {
  id: number
  kind: ToastKind
  message: string
  /** Server machine code (errors only) — e.g. invalid_serial, conflict. */
  code?: string
  /** X-Request-ID (errors only) — the "copy" action target, greppable in logs. */
  requestId?: string
}

// Auto-dismiss windows (ms). Errors are STICKY (0) so the operator can read +
// copy the request id; the rest fade.
const TTL: Record<ToastKind, number> = {
  success: 4000,
  info: 4000,
  warn: 6000,
  error: 0,
}

class ToastStore {
  toasts = $state<Toast[]>([])
  #seq = 0
  #timers = new Map<number, ReturnType<typeof setTimeout>>()

  #push(kind: ToastKind, message: string, extra: Partial<Toast> = {}): number {
    const id = ++this.#seq
    this.toasts = [...this.toasts, { id, kind, message, ...extra }]
    const ttl = TTL[kind]
    if (ttl > 0) {
      this.#timers.set(
        id,
        setTimeout(() => this.dismiss(id), ttl),
      )
    }
    return id
  }

  success(message: string): number {
    return this.#push('success', message)
  }

  info(message: string): number {
    return this.#push('info', message)
  }

  warn(message: string): number {
    return this.#push('warn', message)
  }

  /** Surface a failure. An ApiError auto-carries its code + requestId; a bare
   *  string is shown as-is. */
  error(err: ApiError | string): number {
    if (err instanceof ApiError) {
      return this.#push('error', err.message, { code: err.code, requestId: err.requestId ?? undefined })
    }
    return this.#push('error', err)
  }

  dismiss(id: number): void {
    const t = this.#timers.get(id)
    if (t) {
      clearTimeout(t)
      this.#timers.delete(id)
    }
    this.toasts = this.toasts.filter((x) => x.id !== id)
  }

  clear(): void {
    for (const t of this.#timers.values()) clearTimeout(t)
    this.#timers.clear()
    this.toasts = []
  }
}

export const notify = new ToastStore()
