// Dirty-state guard for editors (design 19 D19.15). The A23 Berry and A25 FaaS
// editors opt in so a routine nav or a session teardown does not silently destroy
// unsaved RCE/render source. The draft persistence (saveDraft/loadDraft/clearDraft)
// lives in ./draft (sv-router-free, node-testable) and is re-exported here for one
// import site; this module adds the sv-router blockNavigation hook (browser-only).

import { blockNavigation } from 'sv-router'

export { draftKey, saveDraft, loadDraft, clearDraft } from './draft'

const DEFAULT_CONFIRM = 'You have unsaved changes — leave anyway?'

/**
 * Block a tab close + an in-app navigation while isDirty() is true. Call from a
 * component $effect; the returned cleanup is run when the buffer is destroyed.
 * sv-router's blockNavigation: a callback returning false BLOCKS — so a clean
 * buffer returns true (allow), a dirty one blocks the unload and confirms the
 * in-app nav.
 */
export function useDirtyGuard(isDirty: () => boolean, confirmMessage: string = DEFAULT_CONFIRM): () => void {
  return blockNavigation({
    beforeUnload() {
      return !isDirty() // dirty → false → browser prompts before tab close
    },
    onNavigate() {
      return !isDirty() || confirm(confirmMessage) // dirty → ask; clean → allow
    },
  })
}
