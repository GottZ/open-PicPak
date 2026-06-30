// D19.14 — connection-status display mapping (raw SSE status → the shell
// indicator's three states).

import { describe, expect, it } from 'vitest'
import { connDisplay } from './conn.svelte'

describe('connDisplay', () => {
  it('open → live', () => {
    expect(connDisplay('open')).toEqual({ label: 'live', tone: 'live' })
  })
  it('connecting and error both read as reconnecting (transient, client retries)', () => {
    expect(connDisplay('connecting').tone).toBe('reconnecting')
    expect(connDisplay('error').tone).toBe('reconnecting')
  })
  it('idle and closed read as offline', () => {
    expect(connDisplay('idle').tone).toBe('offline')
    expect(connDisplay('closed').tone).toBe('offline')
  })
})
