import { describe, it, expect } from 'vitest'
import { parseVmErrorLine, vmSyntaxFinding } from './diagnostics'

describe('sim diagnostics', () => {
  it('extracts the VM line from a Berry syntax error', () => {
    expect(parseVmErrorLine("syntax_error: string:2: 'end' expected before 'eof'")).toBe(2)
    expect(parseVmErrorLine('syntax_error: string:14: unexpected symbol near )')).toBe(14)
  })

  it('returns null when no line is embedded', () => {
    expect(parseVmErrorLine('some error without a line')).toBeNull()
    expect(parseVmErrorLine('')).toBeNull()
  })

  // §7 probe (a), pure half: a compile error (rc 1) yields a finding carrying the VM line and the raw
  // message; the inline squiggle at that line is the panel's linter (createSimEditor) — manual with WASM.
  it('vmSyntaxFinding maps a compile error to {line, message}', () => {
    const f = vmSyntaxFinding(1, "syntax_error: string:3: 'end' expected before 'eof'")
    expect(f).toEqual({ line: 3, message: "syntax_error: string:3: 'end' expected before 'eof'" })
  })

  it('falls back to line 1 when a compile error carries no line', () => {
    expect(vmSyntaxFinding(1, 'malformed error')).toEqual({ line: 1, message: 'malformed error' })
  })

  it('yields no finding for ok / runtime / deadline / unavailable results', () => {
    expect(vmSyntaxFinding(0, '')).toBeNull() // compiled fine
    expect(vmSyntaxFinding(2, 'runtime_error: ...')).toBeNull() // runtime, not syntax
    expect(vmSyntaxFinding(3, 'deadline...')).toBeNull() // deadline
    expect(vmSyntaxFinding(-1, 'sim worker: ...')).toBeNull() // no WASM → fail-open
  })
})
