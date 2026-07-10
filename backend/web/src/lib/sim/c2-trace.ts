// The C2 effect trace (design/33 §4.3/§7, W-A33.4). Executes a Berry C2 script against the pure-TS
// 36-stub surface (c2-surface.ts) and returns the ordered list of stub calls that ACTUALLY ran under the
// current scenario values — each with its arguments, return value, and whether the firmware type guard
// passed. This is the "cmd-Trace" of §4.3.
//
// ILLUSTRATION, NOT AUTHORITY (S8, the hard invariant): the trace shows only the path taken under the
// chosen scenario. A severing call hidden behind a branch that is false in the sim but true on-device
// (`if !connected() net_clear() end` while the stub reports connected=true) is CORRECTLY absent from the
// trace — and that is exactly why apply-time severing warnings come from the STATIC lint.ts calls() pass
// (branch-independent, textual), never from this trace. "Statik schlägt Trace." The Trace-UI carries that
// label permanently (SimulatorPanel.svelte).
//
// SUBSET (honest scope, documented so the boundary is explicit — this is not a full Berry VM, that is the
// WASM path): the walker evaluates expression/call statements, `if/elif/else/end` with condition
// evaluation (the S8-relevant control flow), the `!`/`not`/`and`/`or`/`==`/`!=`/`+` operators, nested
// calls, and `var`/assignment binding. `while`/`for`/`do` bodies are walked ONCE (illustrative, never
// iterated — an unbounded loop is the WASM deadline's job, §4.2); `def` bodies are not executed inline.
// Unknown identifiers evaluate to nil. None of this feeds a safety decision — the static pass does.

import { type Arg, type C2Env, type C2Scenario, makeEnv, NIL, bool, str, stubFor, truthy } from './c2-surface'

export interface TraceEntry {
  fn: string
  args: Arg[]
  ret: Arg
  /** firmware guard passed; false = a rejected-type / silent-no-op call (§7 probe b). */
  ok: boolean
  /** catalog class of the stub, for UI grouping + effect-text routing. */
  cls: string
}

export interface TraceResult {
  trace: TraceEntry[]
  /** an unknown bare call the surface lacks (mirrors the static "unknown" lint) — recorded, not thrown. */
  unknown: string[]
}

// ---- tokenizer ----

type Tok =
  | { t: 'id'; v: string }
  | { t: 'kw'; v: string }
  | { t: 'str'; v: string }
  | { t: 'int'; v: number }
  | { t: 'real'; v: number }
  | { t: 'op'; v: string }

const KEYWORDS = new Set([
  'if', 'elif', 'else', 'end', 'while', 'for', 'do', 'def', 'return', 'break',
  'continue', 'var', 'static', 'and', 'or', 'not', 'true', 'false', 'nil', 'import', 'as', 'in',
])

function tokenize(src: string): Tok[] {
  const toks: Tok[] = []
  let i = 0
  const n = src.length
  while (i < n) {
    const c = src[i]
    if (c === '#') {
      while (i < n && src[i] !== '\n') i++
      continue
    }
    if (c === ' ' || c === '\t' || c === '\r' || c === '\n') {
      i++
      continue
    }
    if (c === '"' || c === "'") {
      const quote = c
      i++
      let s = ''
      while (i < n && src[i] !== quote) {
        if (src[i] === '\\' && i + 1 < n) {
          const e = src[i + 1]
          s += e === 'n' ? '\n' : e === 't' ? '\t' : e
          i += 2
        } else {
          s += src[i++]
        }
      }
      i++ // closing quote (tolerate EOF — lint.ts owns unterminated-string diagnostics)
      toks.push({ t: 'str', v: s })
      continue
    }
    if (c >= '0' && c <= '9') {
      let num = ''
      let real = false
      while (i < n && ((src[i] >= '0' && src[i] <= '9') || src[i] === '.')) {
        if (src[i] === '.') {
          if (src[i + 1] === '.') break // range `..` — not a decimal point
          real = true
        }
        num += src[i++]
      }
      toks.push(real ? { t: 'real', v: Number.parseFloat(num) } : { t: 'int', v: Number.parseInt(num, 10) })
      continue
    }
    if (/[A-Za-z_]/.test(c)) {
      let id = ''
      while (i < n && /[A-Za-z0-9_]/.test(src[i])) id += src[i++]
      toks.push(KEYWORDS.has(id) ? { t: 'kw', v: id } : { t: 'id', v: id })
      continue
    }
    // operators (two-char first)
    const two = src.slice(i, i + 2)
    if (two === '==' || two === '!=' || two === '<=' || two === '>=' || two === '..') {
      toks.push({ t: 'op', v: two })
      i += 2
      continue
    }
    toks.push({ t: 'op', v: c })
    i++
  }
  return toks
}

// ---- walker / evaluator ----

class Walker {
  private p = 0
  private vars = new Map<string, Arg>()
  readonly trace: TraceEntry[] = []
  readonly unknown = new Set<string>()

  constructor(
    private readonly toks: Tok[],
    private readonly env: C2Env,
  ) {}

  private peek(): Tok | undefined {
    return this.toks[this.p]
  }
  private next(): Tok | undefined {
    return this.toks[this.p++]
  }
  private isKw(v: string): boolean {
    const t = this.peek()
    return t?.t === 'kw' && t.v === v
  }
  private isOp(v: string): boolean {
    const t = this.peek()
    return t?.t === 'op' && t.v === v
  }

  run(): void {
    while (this.p < this.toks.length) this.statement(true)
  }

  /** One statement. `active` gates side effects — inactive statements are still PARSED (to find `end`). */
  private statement(active: boolean): void {
    const t = this.peek()
    if (!t) return
    if (t.t === 'kw') {
      switch (t.v) {
        case 'if':
          return this.ifStmt(active)
        case 'while':
        case 'do':
          this.next()
          if (t.v === 'while') this.expr(active) // condition (walked once regardless)
          return this.block(active)
        case 'for':
          this.next()
          this.skipTo(new Set(['do', 'end'])) // consume the `i : a .. b` header
          if (this.isKw('do')) this.next()
          return this.block(active)
        case 'def':
          this.next()
          this.skipToLineOrParenEnd()
          return this.block(false) // never execute a def body inline
        case 'var':
        case 'static':
          this.next()
          return this.assignOrExpr(active)
        case 'return':
          this.next()
          if (this.peek() && !this.isKw('end')) this.expr(active)
          return
        case 'break':
        case 'continue':
        case 'import':
        case 'as':
        case 'in':
          this.next()
          return
        case 'end':
        case 'elif':
        case 'else':
          return // handled by the enclosing block
        default:
          this.next()
          return
      }
    }
    this.assignOrExpr(active)
  }

  private ifStmt(active: boolean): void {
    this.next() // 'if'
    let cond = truthy(this.expr(active))
    let branchDone = false
    this.blockUntil(active && cond, new Set(['elif', 'else', 'end']))
    branchDone = active && cond
    while (this.isKw('elif')) {
      this.next()
      const c = truthy(this.expr(active))
      const take = active && !branchDone && c
      this.blockUntil(take, new Set(['elif', 'else', 'end']))
      if (take) branchDone = true
      cond ||= c
    }
    if (this.isKw('else')) {
      this.next()
      this.blockUntil(active && !branchDone, new Set(['end']))
    }
    if (this.isKw('end')) this.next()
  }

  /** Walk statements until one of `stops` (a keyword) is the next token; consumes neither the stop. */
  private blockUntil(active: boolean, stops: Set<string>): void {
    while (this.p < this.toks.length) {
      const t = this.peek()
      if (t?.t === 'kw' && stops.has(t.v)) return
      this.statement(active)
    }
  }

  /** A block terminated by `end` (while/for/do/def). */
  private block(active: boolean): void {
    this.blockUntil(active, new Set(['end']))
    if (this.isKw('end')) this.next()
  }

  private assignOrExpr(active: boolean): void {
    const t = this.peek()
    // `name = expr` binding (single-token lookahead: id followed by a lone `=`, not `==`).
    if (t?.t === 'id' && this.toks[this.p + 1]?.t === 'op' && this.toks[this.p + 1] !== undefined) {
      const op = this.toks[this.p + 1]
      if (op.t === 'op' && op.v === '=') {
        const name = t.v
        this.p += 2
        const v = this.expr(active)
        if (active) this.vars.set(name, v)
        return
      }
    }
    this.expr(active)
  }

  // Expression grammar (low → high precedence): or | and | equality | additive | unary | primary.
  private expr(active: boolean): Arg {
    return this.orExpr(active)
  }

  private orExpr(active: boolean): Arg {
    let left = this.andExpr(active)
    while (this.isKw('or')) {
      this.next()
      const right = this.andExpr(active)
      left = bool(truthy(left) || truthy(right))
    }
    return left
  }

  private andExpr(active: boolean): Arg {
    let left = this.eqExpr(active)
    while (this.isKw('and')) {
      this.next()
      const right = this.eqExpr(active)
      left = bool(truthy(left) && truthy(right))
    }
    return left
  }

  private eqExpr(active: boolean): Arg {
    let left = this.addExpr(active)
    while (this.isOp('==') || this.isOp('!=')) {
      const op = (this.next() as { v: string }).v
      const right = this.addExpr(active)
      const eq = valueEq(left, right)
      left = bool(op === '==' ? eq : !eq)
    }
    return left
  }

  private addExpr(active: boolean): Arg {
    let left = this.unary(active)
    while (this.isOp('+')) {
      this.next()
      const right = this.unary(active)
      // Berry `+` on strings concatenates; on numbers adds. Mixed → string join (illustrative).
      if (left.kind === 'string' || right.kind === 'string') {
        left = str(asText(left) + asText(right))
      } else if (left.kind === 'int' && right.kind === 'int') {
        left = { kind: 'int', value: left.value + right.value }
      } else {
        left = { kind: 'real', value: numOf(left) + numOf(right) }
      }
    }
    return left
  }

  private unary(active: boolean): Arg {
    if (this.isOp('!') || this.isKw('not')) {
      this.next()
      return bool(!truthy(this.unary(active)))
    }
    if (this.isOp('-')) {
      this.next()
      const v = this.unary(active)
      return { kind: 'real', value: -numOf(v) }
    }
    return this.primary(active)
  }

  private primary(active: boolean): Arg {
    const t = this.next()
    if (!t) return NIL
    if (t.t === 'str') return str(t.v)
    if (t.t === 'int') return { kind: 'int', value: t.v }
    if (t.t === 'real') return { kind: 'real', value: t.v }
    if (t.t === 'kw') {
      if (t.v === 'true') return bool(true)
      if (t.v === 'false') return bool(false)
      if (t.v === 'nil') return NIL
      return NIL
    }
    if (t.t === 'op' && t.v === '(') {
      const inner = this.expr(active)
      if (this.isOp(')')) this.next()
      return inner
    }
    if (t.t === 'id') {
      // member access `a.b(...)` — not a bare C2 global; consume the chain, ignore (illustrative).
      if (this.isOp('.')) {
        while (this.isOp('.')) {
          this.next()
          if (this.peek()?.t === 'id') this.next()
        }
        if (this.isOp('(')) this.callArgs(active)
        return NIL
      }
      if (this.isOp('(')) return this.call(t.v, active)
      // bare identifier → a variable binding, else nil.
      return this.vars.get(t.v) ?? NIL
    }
    return NIL
  }

  private callArgs(active: boolean): Arg[] {
    const args: Arg[] = []
    this.next() // '('
    if (this.isOp(')')) {
      this.next()
      return args
    }
    for (;;) {
      args.push(this.expr(active))
      if (this.isOp(',')) {
        this.next()
        continue
      }
      break
    }
    if (this.isOp(')')) this.next()
    return args
  }

  private call(fn: string, active: boolean): Arg {
    const args = this.callArgs(active)
    const def = stubFor(fn)
    if (!def) {
      if (active) this.unknown.add(fn)
      return NIL // unknown global → nil (mirrors the "faults / unknown" static lint; not thrown)
    }
    if (!active) return NIL // dead branch: parsed, but never executed (S8 — no trace, no state change)
    const { ret, ok } = def.run(this.env, args)
    this.trace.push({ fn, args, ret, ok, cls: def.cls })
    return ret
  }

  private skipTo(stops: Set<string>): void {
    while (this.p < this.toks.length) {
      const t = this.peek()
      if (t?.t === 'kw' && stops.has(t.v)) return
      this.next()
    }
  }

  private skipToLineOrParenEnd(): void {
    // consume a def header `name(params)` up to the matching close paren.
    if (this.peek()?.t === 'id') this.next()
    if (this.isOp('(')) {
      let depth = 0
      do {
        const t = this.next()
        if (t?.t === 'op' && t.v === '(') depth++
        else if (t?.t === 'op' && t.v === ')') depth--
      } while (depth > 0 && this.p < this.toks.length)
    }
  }
}

function valueEq(a: Arg, b: Arg): boolean {
  if (a.kind === 'nil' || b.kind === 'nil') return a.kind === b.kind
  if (a.kind === 'string' && b.kind === 'string') return a.value === b.value
  if (a.kind === 'bool' && b.kind === 'bool') return a.value === b.value
  if ((a.kind === 'int' || a.kind === 'real') && (b.kind === 'int' || b.kind === 'real'))
    return numOf(a) === numOf(b)
  return false
}
function numOf(a: Arg): number {
  return a.kind === 'int' || a.kind === 'real' ? a.value : 0
}
function asText(a: Arg): string {
  if (a.kind === 'string') return a.value
  if (a.kind === 'int' || a.kind === 'real') return String(a.value)
  if (a.kind === 'bool') return a.value ? 'true' : 'false'
  return 'nil'
}

/**
 * Run a C2 script under an optional scenario and return the ordered effect trace. Pure and total — any
 * malformed input parses to a partial trace rather than throwing (the editor's lint.ts owns diagnostics).
 */
export function traceC2(source: string, scenario?: C2Scenario, env?: C2Env): TraceResult {
  const walker = new Walker(tokenize(source), env ?? makeEnv(scenario))
  walker.run()
  return { trace: walker.trace, unknown: [...walker.unknown] }
}

export { makeEnv } from './c2-surface'
