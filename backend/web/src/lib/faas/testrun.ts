// Test-run client parsing (Design 25 §4.2) — the pure, node-testable half: decode the framed binary
// response the admin forwards from the supervisor (u32be metaLen | meta JSON | packed(30000) | raw), and
// shape the request body. The actual fetch (which needs the session bearer + returns an ArrayBuffer) lives
// in the component; this module owns the wire format so it is unit-tested without a browser.

import { PACKED_SIZE } from './bwrydecode'

// Source: cmd/faas-supervisor testMeta (testrender.go). log[] + err.msg are FOREIGN strings (the function's
// own output) — the component renders them as TEXT NODES, never {@html} (D19.10 / T2).
export interface TestRunMeta {
  ok: boolean
  status: string // "ok" | "error"
  wake: number
  dither: string
  raw_fmt: string // "rgb" on success, "" on error
  raw_w: number
  raw_h: number
  log: { lvl: string; msg: string }[]
  err: { kind: string; msg: string } | null
}

export interface TestRunResult {
  meta: TestRunMeta
  packed: Uint8Array // always 30000 (an error frame on failure — the preview slot is never empty)
  raw: Uint8Array | null // the pre-pack RGB (360000), present on success only
}

/**
 * Parse the framed test-run response: u32be metaLen | meta JSON | packed(30000) | raw?. Throws on a short
 * or truncated frame (never returns a partial packed frame that would misrender the panel preview).
 */
export function parseTestFrame(buf: ArrayBuffer): TestRunResult {
  if (buf.byteLength < 4) throw new Error('test-run response too short')
  const view = new DataView(buf)
  const metaLen = view.getUint32(0, false) // big-endian, matches encoding/binary.BigEndian
  const bytes = new Uint8Array(buf)
  const packedStart = 4 + metaLen
  if (packedStart + PACKED_SIZE > buf.byteLength) throw new Error('truncated test-run response (no packed frame)')
  const meta = JSON.parse(new TextDecoder().decode(bytes.subarray(4, packedStart))) as TestRunMeta
  const packed = bytes.subarray(packedStart, packedStart + PACKED_SIZE)
  const rawStart = packedStart + PACKED_SIZE
  const raw = rawStart < buf.byteLength ? bytes.subarray(rawStart) : null
  return { meta, packed, raw }
}

// The test-run request shape (POST /api/functions/test-run). fn is the ad-hoc draft (current editor state,
// the fast loop D25.3) or {id}; ctx is the operator-chosen device context.
export interface TestRunFn {
  id?: number
  source?: string
  dither?: string
  secret_bindings?: string[]
  egress_allow?: string[]
  limits?: { timeout_ms?: number; mem_mb?: number }
}
export interface TestRunCtx {
  serial: string
  trigger: string
  now?: string
  payload?: unknown
}

export function buildTestRunBody(fn: TestRunFn, ctx: TestRunCtx): string {
  return JSON.stringify({ fn, ctx })
}
