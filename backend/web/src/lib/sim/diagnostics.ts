// Turning a Berry VM error into an editor diagnostic (design/33 §4.4 — sim_compile_only as the third
// linter() source). Pure and CM6-free so the line extraction — the substance of "syntax error → a
// diagnostic carrying the VM line" (§7 probe a) — is node-testable without the WASM VM (which needs the
// emsdk build; the end-to-end squiggle is manual, examples/diagnostics.test.ts covers the parse).
//
// The wrapper (firmware/sim/sim_main.c capture_error) formats a failed be_loadstring as "<type>: <msg>",
// where Berry's compiler embeds the source line as ":<n>:" — e.g. "syntax_error: string:2: 'end'
// expected before 'eof'". We take the FIRST such group as the offending line.

export interface VmSyntaxFinding {
  /** 1-based source line the VM blamed; falls back to 1 when the message carries no line. */
  line: number
  /** The raw VM message, shown verbatim (a text node — never {@html}, D19.10). */
  message: string
}

/** Extract the 1-based source line Berry embedded in an error message, or null when absent. */
export function parseVmErrorLine(err: string): number | null {
  const m = err.match(/:(\d+):/)
  if (!m) return null
  const n = Number.parseInt(m[1], 10)
  return Number.isFinite(n) && n >= 1 ? n : null
}

/**
 * Map a sim_compile_only result to a syntax finding, or null when the source compiled (rc 0) or the
 * simulator is unavailable (rc < 0 — fail-open: no WASM ⇒ no VM diagnostics, the editor keeps working).
 * Only rc 1 (compile error) yields a finding; a run's rc 2/3 are runtime/deadline, not syntax.
 */
export function vmSyntaxFinding(rc: number, err: string): VmSyntaxFinding | null {
  if (rc !== 1) return null
  return { line: parseVmErrorLine(err) ?? 1, message: err }
}
