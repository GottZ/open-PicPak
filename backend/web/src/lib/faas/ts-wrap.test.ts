import { describe, expect, it } from 'vitest'
import type { Diagnostic } from '@codemirror/lint'
import {
  OFFSET,
  PREAMBLE,
  clampToBuffer,
  fromEnvPos,
  remapDiagnostic,
  toEnvPos,
  wrapSource,
} from './ts-wrap'

describe('wrapSource', () => {
  it('is length-preserving so the buffer→wrapped offset is the constant OFFSET', () => {
    const samples = [
      '',
      'export default async (ctx, cap) => ({ image: null })',
      'const x = 1\nexport   default\tasync (ctx, cap) => ({ image: null })',
      '// no export default here\nconst y = 2',
    ]
    for (const code of samples) {
      expect(wrapSource(code).length).toBe(OFFSET + code.length)
      expect(wrapSource(code).startsWith(PREAMBLE)).toBe(true)
    }
  })

  it('rewrites the leading `export default ` into the typed assignment', () => {
    const body = wrapSource('export default async (ctx, cap) => ({ image: null })').slice(OFFSET)
    expect(body.startsWith('__picpakFn =')).toBe(true)
    // the arrow expression survives verbatim after the (space-padded) assignment
    expect(body).toContain('async (ctx, cap) => ({ image: null })')
    expect(body).not.toContain('export default')
  })

  it('passes code through untouched (after the preamble) when there is no export default', () => {
    const code = 'const y = 2 // partial, still typing'
    expect(wrapSource(code).slice(OFFSET)).toBe(code)
  })
})

describe('position mapping', () => {
  it('round-trips buffer↔env positions', () => {
    expect(fromEnvPos(toEnvPos(17))).toBe(17)
    expect(toEnvPos(0)).toBe(OFFSET)
  })

  it('clamps env positions into the buffer range', () => {
    expect(clampToBuffer(OFFSET - 5, 100)).toBe(0) // inside the preamble → 0
    expect(clampToBuffer(OFFSET + 40, 100)).toBe(40)
    expect(clampToBuffer(OFFSET + 500, 100)).toBe(100) // past the end → clamped
  })
})

describe('remapDiagnostic', () => {
  const mk = (from: number, to: number): Diagnostic => ({ from, to, severity: 'error', message: 'x' })

  it('shifts a user-code diagnostic back by OFFSET', () => {
    const d = remapDiagnostic(mk(OFFSET + 6, OFFSET + 11), 40)
    expect(d).toEqual({ from: 6, to: 11, severity: 'error', message: 'x' })
  })

  it('drops a diagnostic that lives entirely in the preamble scaffold', () => {
    expect(remapDiagnostic(mk(2, OFFSET - 1), 40)).toBeNull()
  })

  it('clamps a boundary-spanning diagnostic to the buffer', () => {
    const d = remapDiagnostic(mk(OFFSET - 3, OFFSET + 200), 40)
    expect(d).toEqual({ from: 0, to: 40, severity: 'error', message: 'x' })
  })
})
