// CodeMirror 6 EditorView factory (Design 23 §4.3, extended A33 §4.1) — the ONE place that imports CM6,
// so the Svelte component stays thin and the dep surface is contained. Editor intelligence is layered
// here from the pure Berry modules: manifest-driven hover docs (hover.ts), inline lint diagnostics
// (@codemirror/lint fed by lint.ts + findingRange), and snippet completion (completion.ts). Still no web
// worker and no eval → fits Doc 19's CSP (script-src 'self') with no CSP delta (O1).

import { EditorState } from '@codemirror/state'
import { EditorView, keymap, lineNumbers, drawSelection, highlightActiveLine, hoverTooltip } from '@codemirror/view'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { closeBrackets, closeBracketsKeymap, completionKeymap } from '@codemirror/autocomplete'
import { bracketMatching } from '@codemirror/language'
import { linter, lintGutter, forceLinting, type Diagnostic } from '@codemirror/lint'
import { berry } from './berryMode'
import { berryAutocomplete } from './completion'
import { lint, findingRange } from './lint'
import { hoverDoc } from './hover'
import type { Manifest } from './catalog'

export interface BerryEditorHandle {
  getDoc(): string
  /** Replace the whole document without re-mounting (a template load, A30-W7). */
  setDoc(doc: string): void
  destroy(): void
}

const editorTheme = EditorView.theme({
  '&': { fontSize: '13px', border: '1px solid var(--border)', borderRadius: '6px' },
  '.cm-content': { fontFamily: 'var(--mono, ui-monospace, monospace)' },
  '&.cm-focused': { outline: 'none', borderColor: 'var(--accent)' },
  '.cm-scroller': { maxHeight: '40vh' },
  '.cm-berry-hover': { padding: '0.35rem 0.5rem', maxWidth: '32rem', fontSize: '12px', lineHeight: '1.4' },
  '.cm-berry-hover-sig': { fontFamily: 'var(--mono, ui-monospace, monospace)', fontWeight: '600', marginBottom: '0.2rem' },
})

const IDENT = /[A-Za-z0-9_]/

/**
 * Manifest-driven hover docs (A33 §4.1.1): resolve the identifier under the pointer via the pure
 * hoverDoc(), and render its lines as TEXT NODES only (D19.10 — catalog docs are operator-influenced).
 */
function berryHover(manifest: Manifest) {
  return hoverTooltip((view, pos) => {
    const line = view.state.doc.lineAt(pos)
    const text = line.text
    const rel = pos - line.from
    let start = rel
    while (start > 0 && IDENT.test(text[start - 1])) start--
    let end = rel
    while (end < text.length && IDENT.test(text[end])) end++
    if (start === end) return null

    const hd = hoverDoc(text.slice(start, end), manifest)
    if (!hd) return null

    return {
      pos: line.from + start,
      end: line.from + end,
      above: true,
      create() {
        const dom = document.createElement('div')
        dom.className = 'cm-berry-hover'
        hd.lines.forEach((ln, i) => {
          const row = document.createElement('div')
          row.textContent = ln
          if (i === 0) row.className = 'cm-berry-hover-sig'
          dom.appendChild(row)
        })
        return { dom }
      },
    }
  })
}

/**
 * Inline diagnostics (A33 §4.1.2): the SAME lint.ts findings the list under the editor shows, mapped to
 * CM6 ranges via the pure findingRange() (call findings underline the callee; balance/string fall back
 * to the line start). @codemirror/lint's built-in debounce carries the keystroke cadence.
 */
function berryLinter(manifest: Manifest) {
  return linter((view) => {
    const doc = view.state.doc.toString()
    return lint(doc, manifest).map((f): Diagnostic => {
      const { from, to } = findingRange(f, doc)
      return { from, to, severity: f.severity, message: f.message }
    })
  })
}

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
      berryHover(opts.manifest),
      berryLinter(opts.manifest),
      lintGutter(),
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
    setDoc: (doc: string) => {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } })
    },
    destroy: () => view.destroy(),
  }
}

export interface SimEditorHandle {
  getDoc(): string
  /** Replace the whole document (a "load example", A33 §4.3). */
  setDoc(doc: string): void
  /** Force the VM linter to re-run — call once the WASM idle-loads so existing content gets checked. */
  refreshLint(): void
  destroy(): void
}

/** A VM syntax finding from sim_compile_only, or null (compiled / no simulator). Mirrors diagnostics.ts. */
export type VmCheck = (doc: string) => Promise<{ line: number; message: string } | null>

/**
 * A render-script editor for the Berry-WASM simulator (A33 §4.3/§4.4). This is deliberately NOT the C2
 * command surface (createBerryEditor): render scripts call fb.* drawing functions, which the C2 catalog
 * classes as `forbidden` — the static C2 lint would flag every fb call, so it is absent here. The ONLY
 * diagnostic is the real VM's compile check (sim_compile_only, the third linter() source of A33's editor
 * intelligence), supplied via `vmCheck`. It returns null (⇒ no squiggle) until the WASM idle-loads and
 * whenever the simulator is unavailable — fail-open: on-device stays authoritative (BerryEditor doctrine).
 * Still no eval and (bar the Worker the caller owns) no CSP delta beyond E-A33-2's worker-src.
 */
export function createSimEditor(opts: {
  parent: HTMLElement
  doc: string
  onChange: (doc: string) => void
  vmCheck: VmCheck
}): SimEditorHandle {
  const vmLinter = linter(async (view): Promise<Diagnostic[]> => {
    const doc = view.state.doc.toString()
    const r = await opts.vmCheck(doc)
    if (!r) return []
    // No column from the VM message → underline the whole blamed line (findingRange's col-less fallback).
    const { from, to } = findingRange({ severity: 'error', kind: 'string', line: r.line, message: r.message }, doc)
    return [{ from, to, severity: 'error', message: r.message }]
  })

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
      vmLinter,
      lintGutter(),
      keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...historyKeymap, ...completionKeymap, indentWithTab]),
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
    setDoc: (doc: string) => {
      view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: doc } })
    },
    refreshLint: () => forceLinting(view),
    destroy: () => view.destroy(),
  }
}
