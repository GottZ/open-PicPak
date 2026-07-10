// Berry structural linter (Design 23 §4.3 / D23.5) — best-effort, client-side, pre-flight ONLY.
//
// It is NOT a Berry parser and does not claim to be: the authoritative compile check is on-device, and
// even THAT failure never returns to the server (the cursor advances regardless, §2.5). So this catches
// the structural mistakes that are cheap to catch in the browser and expensive to discover via a silent
// cursor-advance: block/bracket/string imbalance, calls to names the C2 executor lacks (unknown +
// did-you-mean), calls to render/policy-phase names (forbidden), and calls to severing capabilities.
//
// Discipline: strip comments + strings to spaces FIRST (preserving offsets/newlines) so keywords inside
// them never count — then scan the stripped code. A name in call position is classified against the
// catalog; balance is a stack match over the stripped code.

import { type Manifest, byName, phaseOf, RISK_SEVERING } from './catalog'

export type Severity = 'error' | 'warning'
export type LintKind = 'balance' | 'string' | 'forbidden' | 'unknown' | 'severing'

export interface Finding {
  severity: Severity
  kind: LintKind
  /** 1-based line; the editor renders it as "line N". */
  line: number
  /**
   * 1-based column of the finding's start, when the pass knows it (calls() derives it from the match
   * offset). Absent for whole-line findings (balance/string) — those fall back to the line start in
   * findingRange, never a throw.
   */
  col?: number
  /** 1-based column one past the finding's end (col + name length for calls()). */
  endCol?: number
  message: string
}

// Block keywords that REQUIRE a matching `end` in Berry. elif/else are continuations, not openers.
// Conservative on purpose: a keyword that does NOT need `end` here would false-positive; omitting one
// that does only misses an imbalance (a false negative, the safe direction). These six are unambiguous.
const BLOCK_OPENERS = new Set(['if', 'while', 'for', 'def', 'do', 'class'])

const OPEN_BRACKETS: Record<string, string> = { ')': '(', ']': '[', '}': '{' }

/** Lint a Berry C2 script against the served catalog. Returns findings sorted by line. */
export function lint(script: string, manifest: Manifest): Finding[] {
  const findings: Finding[] = []
  const { code, unterminatedStringLine } = stripCommentsAndStrings(script)

  if (unterminatedStringLine !== null) {
    findings.push({
      severity: 'error',
      kind: 'string',
      line: unterminatedStringLine,
      message: 'unterminated string literal',
    })
  }

  findings.push(...balance(code))
  findings.push(...calls(code, manifest))

  return findings.sort((a, b) => a.line - b.line || a.kind.localeCompare(b.kind))
}

/** Replace comment + string spans with spaces (newlines preserved) so token/balance scans ignore them. */
export function stripCommentsAndStrings(src: string): { code: string; unterminatedStringLine: number | null } {
  const out: string[] = []
  let unterminated: number | null = null
  let line = 1
  let i = 0
  const n = src.length
  const blank = (ch: string) => out.push(ch === '\n' ? '\n' : ' ')

  while (i < n) {
    const ch = src[i]
    if (ch === '\n') {
      out.push('\n')
      line++
      i++
      continue
    }
    // block comment #- ... -#
    if (ch === '#' && src[i + 1] === '-') {
      blank('#')
      blank('-')
      i += 2
      while (i < n && !(src[i] === '-' && src[i + 1] === '#')) {
        if (src[i] === '\n') {
          out.push('\n')
          line++
        } else {
          out.push(' ')
        }
        i++
      }
      if (i < n) {
        blank('-')
        blank('#')
        i += 2
      }
      continue
    }
    // line comment #
    if (ch === '#') {
      while (i < n && src[i] !== '\n') {
        out.push(' ')
        i++
      }
      continue
    }
    // string "..." or '...'
    if (ch === '"' || ch === "'") {
      const quote = ch
      const startLine = line
      out.push(' ')
      i++
      let closed = false
      while (i < n) {
        const c = src[i]
        if (c === '\\') {
          out.push(' ')
          if (src[i + 1] === '\n') {
            out.push('\n')
            line++
          } else {
            out.push(' ')
          }
          i += 2
          continue
        }
        if (c === '\n') break // strings do not span lines here → unterminated
        out.push(' ')
        i++
        if (c === quote) {
          closed = true
          break
        }
      }
      if (!closed && unterminated === null) unterminated = startLine
      continue
    }
    out.push(ch)
    i++
  }
  return { code: out.join(''), unterminatedStringLine: unterminated }
}

/** Block (`end`-delimited) + bracket balance over the stripped code. Stack-based → precise lines. */
function balance(code: string): Finding[] {
  const findings: Finding[] = []
  const blocks: { kw: string; line: number }[] = []
  const brackets: { ch: string; line: number }[] = []
  let line = 1
  let i = 0
  const n = code.length

  while (i < n) {
    const ch = code[i]
    if (ch === '\n') {
      line++
      i++
      continue
    }
    if (ch === '(' || ch === '[' || ch === '{') {
      brackets.push({ ch, line })
      i++
      continue
    }
    if (ch === ')' || ch === ']' || ch === '}') {
      const want = OPEN_BRACKETS[ch]
      const top = brackets[brackets.length - 1]
      if (!top || top.ch !== want) {
        findings.push({ severity: 'error', kind: 'balance', line, message: `unmatched '${ch}'` })
      } else {
        brackets.pop()
      }
      i++
      continue
    }
    // identifier / keyword
    if (/[A-Za-z_]/.test(ch)) {
      let j = i + 1
      while (j < n && /[A-Za-z0-9_]/.test(code[j])) j++
      const word = code.slice(i, j)
      if (BLOCK_OPENERS.has(word)) {
        blocks.push({ kw: word, line })
      } else if (word === 'end') {
        if (blocks.length === 0) {
          findings.push({ severity: 'error', kind: 'balance', line, message: "unexpected 'end' (no open block)" })
        } else {
          blocks.pop()
        }
      }
      i = j
      continue
    }
    i++
  }

  for (const b of blocks) {
    findings.push({
      severity: 'error',
      kind: 'balance',
      line: b.line,
      message: `unclosed '${b.kw}' block — missing 'end'`,
    })
  }
  for (const br of brackets) {
    findings.push({ severity: 'error', kind: 'balance', line: br.line, message: `unclosed '${br.ch}'` })
  }
  return findings
}

// A call site: an identifier immediately followed by '(', NOT preceded by '.' (so json.load is a member
// call, not a bare global). Captures the callee name; the line is derived from the match offset.
const CALL_RE = /([A-Za-z_]\w*)\s*\(/g
const DEF_RE = /\bdef\s+([A-Za-z_]\w*)/g

/** Classify every bare call against the catalog: forbidden / unknown(+suggest) / severing. */
function calls(code: string, manifest: Manifest): Finding[] {
  const findings: Finding[] = []
  const known = byName(manifest)
  const builtins = new Set(manifest.builtins)
  const candidates = nonForbiddenNames(manifest)

  // user-defined functions are legitimate call targets — collect them first.
  const userDefs = new Set<string>()
  for (const m of code.matchAll(DEF_RE)) userDefs.add(m[1])

  for (const m of code.matchAll(CALL_RE)) {
    const name = m[1]
    const at = m.index ?? 0
    // skip member calls (preceded by '.') — obj.method(...) is not a bare global.
    const prev = code.slice(0, at).trimEnd()
    if (prev.endsWith('.')) continue
    if (builtins.has(name) || userDefs.has(name)) continue

    const cap = known.get(name)
    const line = lineOf(code, at)
    // the match offset is the callee's start → precise 1-based columns spanning the name, so the CM6
    // linter underlines the call itself (A33 §4.1.2), not the whole line.
    const col = colOf(code, at)
    const endCol = col + name.length
    if (cap && cap.class !== 'forbidden') {
      if (cap.risk === RISK_SEVERING) {
        findings.push({
          severity: 'warning',
          kind: 'severing',
          line,
          col,
          endCol,
          message: `${name}() is severing — it can cut the device's own C2 path (USB recovery only).`,
        })
      }
      continue
    }
    if (cap && cap.class === 'forbidden') {
      findings.push({
        severity: 'warning',
        kind: 'forbidden',
        line,
        col,
        endCol,
        message:
          `${name}() is a ${phaseOf(cap)} capability — not in the C2 executor; it faults at runtime ` +
          `and the cursor still advances (no error surfaced).`,
      })
      continue
    }
    // unknown global call → did-you-mean against the real surface.
    const hint = suggest(name, candidates)
    findings.push({
      severity: 'warning',
      kind: 'unknown',
      line,
      col,
      endCol,
      message: hint
        ? `${name}() is not a C2 capability — did you mean ${hint}()?`
        : `${name}() is not a C2 capability.`,
    })
  }
  return findings
}

function nonForbiddenNames(m: Manifest): string[] {
  return m.capabilities.filter((c) => c.class !== 'forbidden').map((c) => c.name)
}

/**
 * Nearest real capability for an unknown call. The §2.3↔13b divergences are containment, not edit
 * distance: sleep→device_sleep (catalog name contains the token), set_wifi_add→wifi_add (token contains
 * the catalog name). Levenshtein ≤2 catches plain typos (rebot→reboot). Containment wins over distance.
 */
export function suggest(token: string, candidates: string[]): string | null {
  let containment: string | null = null
  let best: { name: string; d: number } | null = null
  for (const c of candidates) {
    if (c.includes(token) || token.includes(c)) {
      if (containment === null || c.length < containment.length) containment = c
    }
    const d = levenshtein(token, c)
    if (d <= 2 && (best === null || d < best.d || (d === best.d && c.length < best.name.length))) {
      best = { name: c, d }
    }
  }
  return containment ?? (best ? best.name : null)
}

function levenshtein(a: string, b: string): number {
  const m = a.length
  const k = b.length
  if (m === 0) return k
  if (k === 0) return m
  let prev = Array.from({ length: k + 1 }, (_, j) => j)
  let cur = new Array<number>(k + 1)
  for (let i = 1; i <= m; i++) {
    cur[0] = i
    for (let j = 1; j <= k; j++) {
      const cost = a[i - 1] === b[j - 1] ? 0 : 1
      cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + cost)
    }
    ;[prev, cur] = [cur, prev]
  }
  return prev[k]
}

function lineOf(code: string, index: number): number {
  let line = 1
  for (let i = 0; i < index && i < code.length; i++) if (code[i] === '\n') line++
  return line
}

/** 1-based column of `index` (offset from the start of its line). */
function colOf(code: string, index: number): number {
  let start = index
  while (start > 0 && code[start - 1] !== '\n') start--
  return index - start + 1
}

/** Absolute offset of the 1-based line's first character. Clamped to doc length. */
function lineStartOffset(doc: string, line: number): number {
  if (line <= 1) return 0
  let seen = 1
  for (let i = 0; i < doc.length; i++) {
    if (doc[i] === '\n') {
      seen++
      if (seen === line) return i + 1
    }
  }
  return doc.length
}

/** Absolute offset of the newline (or doc end) terminating the line that starts at `from`. */
function lineEndOffset(doc: string, from: number): number {
  let i = from
  while (i < doc.length && doc[i] !== '\n') i++
  return i
}

/**
 * Map a Finding to an absolute {from,to} range for a CM6 diagnostic — the CM6-free half of the linter
 * wiring (editor.ts adds the severity/message). A finding WITH col/endCol underlines exactly the callee
 * span; a finding WITHOUT col (balance/string) falls back to the whole line from its start (§4.1.2:
 * "ohne col → Zeilenanfang-Fallback") — never a throw or an out-of-range offset. All results are clamped
 * into [0, doc.length] so CM6 never rejects a stale range mid-edit.
 */
export function findingRange(f: Finding, doc: string): { from: number; to: number } {
  const len = doc.length
  const lineStart = lineStartOffset(doc, f.line)
  if (f.col === undefined) {
    return { from: Math.min(lineStart, len), to: Math.min(lineEndOffset(doc, lineStart), len) }
  }
  const from = Math.min(lineStart + (f.col - 1), len)
  const to = Math.min(lineStart + ((f.endCol ?? f.col) - 1), len)
  return { from, to: Math.max(from, to) }
}
