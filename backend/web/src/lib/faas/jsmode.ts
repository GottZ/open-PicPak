// A thin CodeMirror 6 JavaScript mode (Design 25 §4.5): keyword / string / template / comment / number
// highlight via a StreamLanguage — NO @codemirror/lang-javascript dependency (Doc 25 §7: one shared CM6
// pin with Doc 23, zero new deps). Display-only; the authoritative correctness check is the test-run typed
// err (W3), not this highlighter. CSP-clean: StreamLanguage runs on the main thread, no worker, no eval
// (Doc 23 O1 — fits the existing script-src 'self').

import { StreamLanguage, HighlightStyle, syntaxHighlighting, LanguageSupport } from '@codemirror/language'
import { tags as t } from '@lezer/highlight'

const KEYWORDS = new Set([
  'const', 'let', 'var', 'function', 'return', 'if', 'else', 'for', 'while', 'do', 'switch',
  'case', 'default', 'break', 'continue', 'new', 'await', 'async', 'try', 'catch', 'finally',
  'throw', 'typeof', 'instanceof', 'in', 'of', 'this', 'class', 'extends', 'super', 'export',
  'import', 'from', 'as', 'yield', 'delete', 'void', 'null', 'undefined', 'true', 'false',
])

interface JSState {
  inBlockComment: boolean
  inTemplate: boolean
}

// Consume characters until a `*/` closes the block comment on this line; returns true if it closed.
function consumeBlockComment(stream: { eol(): boolean; next(): string | void }): boolean {
  let prev = ''
  while (!stream.eol()) {
    const c = stream.next() ?? ''
    if (prev === '*' && c === '/') return true
    prev = c
  }
  return false
}

// Consume a template literal until an unescaped backtick; returns true if it closed on this line.
function consumeTemplate(stream: { eol(): boolean; next(): string | void }): boolean {
  while (!stream.eol()) {
    const c = stream.next() ?? ''
    if (c === '\\') {
      stream.next()
      continue
    }
    if (c === '`') return true
  }
  return false
}

const jsStream = StreamLanguage.define<JSState>({
  startState: () => ({ inBlockComment: false, inTemplate: false }),
  token(stream, state) {
    if (state.inBlockComment) {
      if (consumeBlockComment(stream)) state.inBlockComment = false
      return 'comment'
    }
    if (state.inTemplate) {
      if (consumeTemplate(stream)) state.inTemplate = false
      return 'string'
    }
    if (stream.eatSpace()) return null
    const ch = stream.peek() ?? ''

    // comments: // line, /* block */
    if (ch === '/') {
      stream.next()
      if (stream.eat('/')) {
        stream.skipToEnd()
        return 'comment'
      }
      if (stream.eat('*')) {
        if (!consumeBlockComment(stream)) state.inBlockComment = true
        return 'comment'
      }
      return null // division / regex — not highlighted
    }
    // template literal
    if (ch === '`') {
      stream.next()
      if (!consumeTemplate(stream)) state.inTemplate = true
      return 'string'
    }
    // single/double-quoted strings
    if (ch === '"' || ch === "'") {
      const quote = stream.next()
      let escaped = false
      while (!stream.eol()) {
        const c = stream.next()
        if (c === '\\' && !escaped) {
          escaped = true
          continue
        }
        if (c === quote && !escaped) break
        escaped = false
      }
      return 'string'
    }
    // numbers
    if (/[0-9]/.test(ch)) {
      stream.eatWhile(/[0-9._xXa-fA-FboeE+-]/)
      return 'number'
    }
    // identifiers / keywords
    if (/[A-Za-z_$]/.test(ch)) {
      stream.eatWhile(/[A-Za-z0-9_$]/)
      return KEYWORDS.has(stream.current()) ? 'keyword' : 'variableName'
    }
    stream.next()
    return null
  },
})

// Token → highlight tag; colors come from the app CSS variables (style-src 'unsafe-inline' OK).
const jsHighlight = HighlightStyle.define([
  { tag: t.keyword, color: 'var(--accent)' },
  { tag: t.comment, color: 'var(--fg-muted)', fontStyle: 'italic' },
  { tag: t.string, color: 'var(--ok, #4caf50)' },
  { tag: t.number, color: 'var(--warn, #d08770)' },
])

/** The JavaScript language + highlight as one CodeMirror extension. */
export function javascript(): LanguageSupport {
  return new LanguageSupport(jsStream, [syntaxHighlighting(jsHighlight)])
}
