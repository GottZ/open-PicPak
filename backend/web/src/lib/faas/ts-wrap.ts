// ts-wrap.ts — pure text/position glue between the CodeMirror buffer and the in-editor TypeScript
// program. No DOM, no worker, no TypeScript imports → fully unit-testable in node (ts-wrap.test.ts).
//
// WHY a wrapper: the operator writes `export default async (ctx, cap) => {…}` with UNANNOTATED params,
// so `ctx`/`cap` are otherwise implicit `any` and nothing is checked against the contract. To type them
// we assign that arrow to a variable of the contract function type — TypeScript then contextually types
// the parameters (mirrors what backend/worker/runtime.ts does at runtime with `__default = …`). We
// prepend a fixed PREAMBLE and rewrite the leading `export default ` into that assignment. The rewrite is
// LENGTH-PRESERVING (padded with spaces), so every buffer offset shifts by exactly PREAMBLE.length — the
// whole position mapping is a single constant, which is what the tests pin.

import type { Diagnostic } from '@codemirror/lint'

/** vfs paths — kept here (a pure, TS-compiler-free module) so the main-thread client can import them
 * without dragging the TypeScript compiler into its chunk. */
export const OPERATOR_PATH = '/operator.ts'
export const CONTRACT_PATH = '/picpak-faas.d.ts'

export const PREAMBLE =
  'let __picpakFn: (ctx: FaasCtx, cap: FaasCap) => FaasResult | Promise<FaasResult>;\n'

/** Constant buffer→wrapped offset: the wrapped file is PREAMBLE + (length-preserving) body. */
export const OFFSET = PREAMBLE.length

const EXPORT_DEFAULT = /export\s+default\s+/
// 12 chars; the matched `export default ` is always ≥ 15 (export + ≥1 ws + default + ≥1 ws), so ASSIGN
// always fits inside the matched span and can be right-padded with spaces to the exact same length.
const ASSIGN = '__picpakFn ='

/**
 * Wrap the operator buffer so its default-export arrow is contextually typed against the contract.
 * The returned string has length exactly OFFSET + code.length (length-preserving rewrite).
 */
export function wrapSource(code: string): string {
  const m = EXPORT_DEFAULT.exec(code)
  if (!m || m.index === undefined) return PREAMBLE + code
  const kw = m[0]
  const repl = ASSIGN.padEnd(kw.length, ' ') // same length as kw → offsets after it are preserved
  const body = code.slice(0, m.index) + repl + code.slice(m.index + kw.length)
  return PREAMBLE + body
}

/** Buffer position → position in the wrapped file handed to the TypeScript service. */
export const toEnvPos = (bufPos: number): number => bufPos + OFFSET

/** Wrapped-file position → buffer position. */
export const fromEnvPos = (envPos: number): number => envPos - OFFSET

/**
 * Map a diagnostic from wrapped-file coords back to buffer coords, dropping any diagnostic that lives
 * entirely inside the PREAMBLE scaffold (never the operator's fault → never a squiggle in their code).
 */
export function remapDiagnostic(d: Diagnostic, bufLen: number): Diagnostic | null {
  const to = d.to - OFFSET
  if (to <= 0) return null // entirely inside the preamble scaffold
  const from = Math.max(0, d.from - OFFSET)
  return { ...d, from, to: Math.min(bufLen, to) }
}

/** Clamp a completion/hover anchor from wrapped-file coords into the valid buffer range. */
export function clampToBuffer(envPos: number, bufLen: number): number {
  return Math.min(bufLen, Math.max(0, fromEnvPos(envPos)))
}
