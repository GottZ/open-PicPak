// CodeMirror 6 EditorView factory (Design 23 §4.3) — the ONE place that imports CM6, so the Svelte
// component stays thin and the dep surface is contained. Basic config only: no web worker, no eval →
// fits Doc 19's CSP (script-src 'self', style-src 'self' 'unsafe-inline') with no CSP delta (O1).

import { EditorState } from '@codemirror/state'
import { EditorView, keymap, lineNumbers, drawSelection, highlightActiveLine } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete'
import { bracketMatching } from '@codemirror/language'
import { berry } from './berryMode'
import { berryAutocomplete } from './completion'
import type { Manifest } from './catalog'

export interface BerryEditorHandle {
  getDoc(): string
  destroy(): void
}

const editorTheme = EditorView.theme({
  '&': { fontSize: '13px', border: '1px solid var(--border)', borderRadius: '6px' },
  '.cm-content': { fontFamily: 'var(--mono, ui-monospace, monospace)' },
  '&.cm-focused': { outline: 'none', borderColor: 'var(--accent)' },
  '.cm-scroller': { maxHeight: '40vh' },
})

/** Mount a Berry editor into `parent`, seeded with `doc`, autocompleting the manifest. */
export function createBerryEditor(opts: {
  parent: HTMLElement
  doc: string
  manifest: Manifest
  onChange: (doc: string) => void
}): BerryEditorHandle {
  const state = EditorState.create({
    doc: opts.doc,
    extensions: [
      lineNumbers(),
      history(),
      drawSelection(),
      highlightActiveLine(),
      bracketMatching(),
      closeBrackets(),
      berry(),
      berryAutocomplete(opts.manifest),
      keymap.of([
        ...closeBracketsKeymap,
        ...defaultKeymap,
        ...historyKeymap,
        ...completionKeymap,
        indentWithTab,
      ]),
      EditorView.lineWrapping,
      EditorView.updateListener.of((u) => {
        if (u.docChanged) opts.onChange(u.state.doc.toString())
      }),
      editorTheme,
    ],
  })
  const view = new EditorView({ state, parent: opts.parent })
  return {
    getDoc: () => view.state.doc.toString(),
    destroy: () => view.destroy(),
  }
}
