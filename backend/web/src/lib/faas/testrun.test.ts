import { describe, it, expect } from 'vitest'
import { parseTestFrame, buildTestRunBody, type TestRunMeta } from './testrun'
import { PACKED_SIZE, RAW_RGB_SIZE } from './bwrydecode'

// Build a framed test-run response (u32be metaLen | meta JSON | packed | raw?) — the exact wire
// writeTestFrame emits, so the parser is golden-locked to the supervisor.
function frame(meta: TestRunMeta, withRaw: boolean): ArrayBuffer {
  const mj = new TextEncoder().encode(JSON.stringify(meta))
  const rawLen = withRaw ? RAW_RGB_SIZE : 0
  const buf = new ArrayBuffer(4 + mj.length + PACKED_SIZE + rawLen)
  const bytes = new Uint8Array(buf)
  new DataView(buf).setUint32(0, mj.length, false)
  bytes.set(mj, 4)
  // packed: fill with a sentinel so we can assert the slice boundary.
  bytes.fill(0xab, 4 + mj.length, 4 + mj.length + PACKED_SIZE)
  if (withRaw) bytes.fill(0xcd, 4 + mj.length + PACKED_SIZE)
  return buf
}

const okMeta: TestRunMeta = {
  ok: true,
  status: 'ok',
  wake: 3600,
  dither: 'none',
  raw_fmt: 'rgb',
  raw_w: 400,
  raw_h: 300,
  log: [{ lvl: 'info', msg: 'rendered' }],
  err: null,
}

describe('parseTestFrame', () => {
  it('splits meta / packed / raw on a success frame', () => {
    const r = parseTestFrame(frame(okMeta, true))
    expect(r.meta.ok).toBe(true)
    expect(r.meta.wake).toBe(3600)
    expect(r.packed.length).toBe(PACKED_SIZE)
    expect(r.packed[0]).toBe(0xab)
    expect(r.raw).not.toBeNull()
    expect(r.raw!.length).toBe(RAW_RGB_SIZE)
    expect(r.raw![0]).toBe(0xcd)
  })

  it('an error frame carries the packed error slot but no raw', () => {
    const errMeta: TestRunMeta = { ...okMeta, ok: false, status: 'error', raw_fmt: '', err: { kind: 'throw', msg: 'boom' } }
    const r = parseTestFrame(frame(errMeta, false))
    expect(r.meta.ok).toBe(false)
    expect(r.meta.err?.kind).toBe('throw')
    expect(r.packed.length).toBe(PACKED_SIZE)
    expect(r.raw).toBeNull()
  })

  it('throws on a short or truncated response', () => {
    expect(() => parseTestFrame(new ArrayBuffer(2))).toThrow()
    // meta present but packed truncated
    const mj = new TextEncoder().encode('{}')
    const buf = new ArrayBuffer(4 + mj.length + 10)
    new DataView(buf).setUint32(0, mj.length, false)
    new Uint8Array(buf).set(mj, 4)
    expect(() => parseTestFrame(buf)).toThrow()
  })

  it('preserves foreign log/err strings verbatim (the component renders them as text, T2)', () => {
    const xss: TestRunMeta = { ...okMeta, log: [{ lvl: 'info', msg: '<img src=x onerror=alert(1)>' }] }
    const r = parseTestFrame(frame(xss, true))
    expect(r.meta.log[0].msg).toBe('<img src=x onerror=alert(1)>')
  })
})

describe('buildTestRunBody', () => {
  it('nests fn + ctx', () => {
    const body = JSON.parse(buildTestRunBody({ source: 'x', dither: 'none' }, { serial: 'S1', trigger: 'render' }))
    expect(body.fn.source).toBe('x')
    expect(body.ctx.serial).toBe('S1')
    expect(body.ctx.trigger).toBe('render')
  })
})
