// D19.15 — editor draft persistence: save/load/clear over localStorage, scoped
// keys, best-effort on a storage failure. (useDirtyGuard wires sv-router
// blockNavigation — exercised live, not unit tested; the draft logic is the part
// with branches worth pinning, and it stays sv-router-free so it loads in node.)

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { clearDraft, draftKey, loadDraft, saveDraft } from './draft'

function memoryStorage(): Storage {
  const store = new Map<string, string>()
  return {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => [...store.keys()][i] ?? null,
    get length() {
      return store.size
    },
  }
}

beforeEach(() => {
  vi.unstubAllGlobals()
  vi.stubGlobal('localStorage', memoryStorage())
})

describe('draft persistence', () => {
  it('scopes keys under a stable prefix', () => {
    expect(draftKey('berry:D2XXXXR')).toBe('picpak.draft.berry:D2XXXXR')
  })

  it('round-trips a draft and clears it', () => {
    saveDraft('berry:D2XXXXR', 'def render() ...')
    expect(loadDraft('berry:D2XXXXR')).toBe('def render() ...')
    clearDraft('berry:D2XXXXR')
    expect(loadDraft('berry:D2XXXXR')).toBeNull()
  })

  it('returns null for an absent draft', () => {
    expect(loadDraft('nope')).toBeNull()
  })

  it('swallows a storage failure (drafting is best-effort, never load-bearing)', () => {
    vi.stubGlobal('localStorage', {
      setItem() {
        throw new Error('quota exceeded')
      },
      getItem() {
        throw new Error('disabled')
      },
      removeItem() {
        throw new Error('disabled')
      },
    } as unknown as Storage)
    expect(() => saveDraft('x', 'y')).not.toThrow()
    expect(loadDraft('x')).toBeNull()
    expect(() => clearDraft('x')).not.toThrow()
  })
})
