// CodeMirror 6 EditorView factory for the FaaS editor (Design 25 §4.5) — the ONE place that imports CM6,
// so the Svelte component stays thin and the dep surface is contained (shared pin with Doc 23). Basic
// config only: no web worker, no eval → fits Doc 19's CSP (script-src 'self', style-src 'unsafe-inline')
// with NO CSP delta (D25.8 / Doc 23 O1). Adds setDoc so the one editor re-seeds when the operator selects
// a different function, and a readOnly mode for the non-admin view-only surface (D25.11).

import { EditorState, Compartment } from '@codemirror/state'
import { EditorView, keymap, lineNumbers, drawSelection, highlightActiveLine } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete'
import { bracketMatching } from '@codemirror/language'
import { javascript } from './jsmode'
import { faasAutocomplete, faasCompletionSource } from './completion'

/** Outcome of an enableTypeService() attempt — 'unavailable' is the named fail-open state (W-A33.3). */
export type TypeServiceState = 'ready' | 'unavailable'

export interface FaasEditorHandle {
  getDoc(): string
  /** Replace the whole document (re-seed on a selection change) without re-mounting. */
  setDoc(doc: string): void
  /** Toggle read-only (a live admin demotion, D25.11) without re-mounting. */
  setReadOnly(ro: boolean): void
  /**
   * Lazily load the TypeScript language service (contract diagnostics + completion + hover) into the
   * running editor, importing the heavy chunk on first call only (idempotent). The editor stays fully
   * functional whether this resolves 'ready' or 'unavailable' — a load failure never breaks editing.
   */
  enableTypeService(): Promise<TypeServiceState>
  destroy(): void
}

const editorTheme = EditorView.theme({
  '&': { fontSize: '13px', border: '1px solid var(--border)', borderRadius: '6px' },
  '.cm-content': { fontFamily: 'var(--mono, ui-monospace, monospace)' },
  '&.cm-focused': { outline: 'none', borderColor: 'var(--accent)' },
  '.cm-scroller': { maxHeight: '48vh' },
})

/**
 * Mount a FaaS JS editor into `parent`, seeded with `doc`, autocompleting the curated scope + the
 * function's bound secrets (read live via `getBound`). `onChange` fires on every doc edit.
 */
export function createFaasEditor(opts: {
  parent: HTMLElement
  doc: string
  getBound: () => readonly string[]
  onChange: (doc: string) => void
  readOnly?: boolean
}): FaasEditorHandle {
  const editable = new Compartment()
  // The TypeScript language service is loaded lazily into this compartment (empty until opt-in), so its
  // chunk never touches the initial bundle.
  const tsService = new Compartment()
  const state = EditorState.create({
    doc: opts.doc,
    extensions: [
      lineNumbers(),
      history(),
      drawSelection(),
      highlightActiveLine(),
      bracketMatching(),
      closeBrackets(),
      javascript(),
      faasAutocomplete(opts.getBound),
      keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...historyKeymap, ...completionKeymap, indentWithTab]),
      EditorView.lineWrapping,
      editable.of(EditorState.readOnly.of(opts.readOnly ?? false)),
      tsService.of([]),
      EditorView.updateListener.of((u) => {
        if (u.docChanged) opts.onChange(u.state.doc.toString())
      }),
      editorTheme,
    ],
  })
  const view = new EditorView({ state, parent: opts.parent })

  let tsDispose: (() => void) | null = null
  let tsPending: Promise<TypeServiceState> | null = null
  let destroyed = false

  return {
    getDoc: () => view.state.doc.toString(),
    setDoc: (doc: string) => {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } })
    },
    setReadOnly: (ro: boolean) => {
      view.dispatch({ effects: editable.reconfigure(EditorState.readOnly.of(ro)) })
    },
    enableTypeService: () => {
      if (tsPending) return tsPending
      tsPending = (async (): Promise<TypeServiceState> => {
        try {
          const { createTsService } = await import('./ts-service-client')
          const svc = await createTsService()
          if (destroyed) {
            svc.dispose()
            return 'unavailable'
          }
          tsDispose = svc.dispose
          // Combine the type-aware source with the static catalog source so bound-secret completions
          // (cap.secrets.*) survive the upgrade. This autocompletion config is added after the base one,
          // so its override wins the combine — hence it must carry BOTH sources.
          view.dispatch({
            effects: tsService.reconfigure([
              ...svc.extensions,
              autocompletion({ override: [svc.completionSource, faasCompletionSource(opts.getBound)] }),
            ]),
          })
          return 'ready'
        } catch {
          return 'unavailable'
        }
      })()
      return tsPending
    },
    destroy: () => {
      destroyed = true
      tsDispose?.()
      view.destroy()
    },
  }
}
