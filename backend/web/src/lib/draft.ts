// Editor draft persistence (design 19 D19.15) — the pure, node-testable half of
// the dirty-state guard, kept free of any sv-router import so it loads in the
// vitest node env (dirtyGuard.svelte.ts adds the sv-router blockNavigation hook).
//
// SECURITY: the bearer key stays in sessionStorage (D19.9); ONLY non-secret
// editor content is drafted to localStorage here — never a key, token, or secret
// value. Callers must pass editor source only.

const DRAFT_PREFIX = 'picpak.draft.'

export function draftKey(scope: string): string {
  return DRAFT_PREFIX + scope
}

/** Persist a draft (best effort — a quota/disabled-storage error is swallowed). */
export function saveDraft(scope: string, content: string): void {
  try {
    localStorage.setItem(draftKey(scope), content)
  } catch {
    /* storage full or disabled — drafting is a convenience, never load-bearing */
  }
}

export function loadDraft(scope: string): string | null {
  try {
    return localStorage.getItem(draftKey(scope))
  } catch {
    return null
  }
}

export function clearDraft(scope: string): void {
  try {
    localStorage.removeItem(draftKey(scope))
  } catch {
    /* no-op */
  }
}
