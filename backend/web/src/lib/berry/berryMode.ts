// A thin CodeMirror 6 Berry mode (Design 23 §4.3): keyword/string/comment/number highlight via a
// StreamLanguage (no Lezer grammar — Berry is small and this is display-only; the linter is the
// load-bearing correctness path, not this). CSP-clean: StreamLanguage runs on the main thread, no
// worker, no eval (O1 — fits the existing script-src 'self').

import { StreamLanguage, HighlightStyle, syntaxHighlighting, LanguageSupport } from '@codemirror/language'
import { tags as t } from '@lezer/highlight'

const KEYWORDS = new Set([
  'if', 'elif', 'else', 'end', 'for', 'while', 'do', 'def', 'return', 'break', 'continue',
  'var', 'static', 'true', 'false', 'nil', 'import', 'as', 'and', 'or', 'not', 'in',
  'class', 'self', 'super', 'try', 'except', 'raise',
])

const berryStream = StreamLanguage.define({
  token(stream) {
    if (stream.eatSpace()) return null
    const ch = stream.peek() ?? ''
    // comments: #- ... -#  and  # ...
    if (ch === '#') {
      stream.next()
      if (stream.eat('-')) {
        // block comment: consume to -# (single-line best effort; multi-line handled by re-entry)
        while (!stream.eol()) {
          if (stream.next() === '-' && stream.peek() === '#') {
            stream.next()
            break
          }
        }
      } else {
        stream.skipToEnd()
      }
      return 'comment'
    }
    // strings
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
      stream.eatWhile(/[0-9._xXa-fA-F]/)
      return 'number'
    }
    // identifiers / keywords
    if (/[A-Za-z_]/.test(ch)) {
      stream.eatWhile(/[A-Za-z0-9_]/)
      const word = stream.current()
      return KEYWORDS.has(word) ? 'keyword' : 'variableName'
    }
    stream.next()
    return null
  },
})

// Token name → highlight tag. Colors come from the app's CSS variables (style-src 'unsafe-inline' OK).
const berryHighlight = HighlightStyle.define([
  { tag: t.keyword, color: 'var(--accent)' },
  { tag: t.comment, color: 'var(--fg-muted)', fontStyle: 'italic' },
  { tag: t.string, color: 'var(--ok, #4caf50)' },
  { tag: t.number, color: 'var(--warn, #d08770)' },
])

/** The Berry language + highlight as one CodeMirror extension. */
export function berry(): LanguageSupport {
  return new LanguageSupport(berryStream, [syntaxHighlighting(berryHighlight)])
}
