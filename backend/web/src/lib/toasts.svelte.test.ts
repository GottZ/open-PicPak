// D19.11 — toast store: kinds, auto-dismiss windows, ApiError surfacing (code +
// requestId), sticky errors, manual dismiss. Fake timers for the auto-dismiss.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { notify } from './toasts.svelte'
import { ApiError } from './api'

beforeEach(() => {
  notify.clear()
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('notify', () => {
  it('pushes a success toast and auto-dismisses after its window', () => {
    notify.success('saved')
    expect(notify.toasts).toHaveLength(1)
    expect(notify.toasts[0]).toMatchObject({ kind: 'success', message: 'saved' })
    vi.advanceTimersByTime(4000)
    expect(notify.toasts).toHaveLength(0)
  })

  it('surfaces an ApiError code + requestId and keeps the error sticky', () => {
    const err = new ApiError(409, 'conflict', 'still referenced', 'req-42', { referenced_by: ['x'] })
    notify.error(err)
    expect(notify.toasts[0]).toMatchObject({
      kind: 'error',
      message: 'still referenced',
      code: 'conflict',
      requestId: 'req-42',
    })
    // errors do NOT auto-dismiss (the operator must be able to copy the id)
    vi.advanceTimersByTime(60_000)
    expect(notify.toasts).toHaveLength(1)
  })

  it('shows a bare-string error as-is', () => {
    notify.error('something broke')
    expect(notify.toasts[0]).toMatchObject({ kind: 'error', message: 'something broke' })
    expect(notify.toasts[0].code).toBeUndefined()
  })

  it('dismisses a toast by id', () => {
    const id = notify.warn('heads up')
    expect(notify.toasts).toHaveLength(1)
    notify.dismiss(id)
    expect(notify.toasts).toHaveLength(0)
  })

  it('keeps multiple toasts distinct by id', () => {
    notify.info('a')
    notify.info('b')
    expect(notify.toasts.map((t) => t.message)).toEqual(['a', 'b'])
    expect(new Set(notify.toasts.map((t) => t.id)).size).toBe(2)
  })
})
