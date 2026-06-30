// D19.17 — read-only affordance contract: an admin sees an enabled control; a
// read-only key sees it DISABLED WITH A REASON (discoverable, never hidden).

import { describe, expect, it } from 'vitest'
import { mutationAffordance } from './readonly'

describe('mutationAffordance', () => {
  it('admin → enabled, no tooltip', () => {
    expect(mutationAffordance(true)).toEqual({ disabled: false, title: '', 'aria-disabled': false })
  })
  it('read-only → disabled with a reason (not hidden)', () => {
    const a = mutationAffordance(false)
    expect(a.disabled).toBe(true)
    expect(a['aria-disabled']).toBe(true)
    expect(a.title).toMatch(/needs admin/i)
  })
})
