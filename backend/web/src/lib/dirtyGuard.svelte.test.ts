// W7 gate (design 29 §7): the FIRST consumer proof for the dirty-state guard
// (design 19 D19.15). dirtyGuard.svelte.ts had 0 consumers, so it had never been
// proven to actually fire — the playlist editor is the first surface to wire it,
// and this pins the contract the editor relies on: while the editor is dirty, an
// in-app navigation MUST route through confirm(), and a clean editor MUST pass
// silently. The RED belt: if the guard let navigation through unconditionally
// (onNavigate → true), the dirty-case assertion below (confirm was called + its
// answer gated the nav) fails — the "navigation goes silently through" regression.
//
// sv-router's blockNavigation touches live router internals, so it is mocked to
// capture the { beforeUnload, onNavigate } callbacks the guard registers; the test
// then drives them exactly as the router would.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// Capture the config object useDirtyGuard hands to blockNavigation.
let captured: { beforeUnload?: () => boolean; onNavigate?: () => boolean } | null = null
const cleanup = vi.fn()

vi.mock('sv-router', () => ({
  blockNavigation: (cfg: { beforeUnload?: () => boolean; onNavigate?: () => boolean }) => {
    captured = cfg
    return cleanup
  },
}))

// Import AFTER the mock is registered (vi.mock is hoisted, so this is safe).
const { useDirtyGuard } = await import('./dirtyGuard.svelte')

let confirmSpy: ReturnType<typeof vi.fn>

beforeEach(() => {
  captured = null
  cleanup.mockClear()
  confirmSpy = vi.fn(() => true)
  vi.stubGlobal('confirm', confirmSpy)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useDirtyGuard — first consumer (playlist editor)', () => {
  it('lets an in-app navigation pass silently while the editor is clean', () => {
    let dirty = false
    useDirtyGuard(() => dirty, 'leave?')
    expect(captured?.onNavigate).toBeTypeOf('function')
    // clean → nav allowed, confirm never shown.
    expect(captured!.onNavigate!()).toBe(true)
    expect(confirmSpy).not.toHaveBeenCalled()
  })

  it('routes an in-app navigation through confirm while the editor is dirty', () => {
    let dirty = false
    useDirtyGuard(() => dirty, 'You have unsaved changes — leave anyway?')
    dirty = true // the operator edited the playlist

    // operator cancels the dialog → navigation is BLOCKED (guard returns false).
    confirmSpy.mockReturnValueOnce(false)
    expect(captured!.onNavigate!()).toBe(false)
    expect(confirmSpy).toHaveBeenCalledWith('You have unsaved changes — leave anyway?')

    // operator accepts → navigation is ALLOWED (guard returns true).
    confirmSpy.mockReturnValueOnce(true)
    expect(captured!.onNavigate!()).toBe(true)
  })

  it('blocks a tab close (beforeUnload → false) only while dirty', () => {
    let dirty = false
    useDirtyGuard(() => dirty)
    expect(captured!.beforeUnload!()).toBe(true) // clean → allow unload
    dirty = true
    expect(captured!.beforeUnload!()).toBe(false) // dirty → browser prompts
  })

  it('returns the router cleanup so the editor can unregister on destroy', () => {
    const off = useDirtyGuard(() => false)
    expect(off).toBe(cleanup)
  })
})
