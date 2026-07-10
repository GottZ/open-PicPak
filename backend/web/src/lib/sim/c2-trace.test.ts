// The C2 trace engine — the four §7 W-A33.4 negative probes, each run pure (no WASM VM: the C2 surface
// is TS, faithful to the firmware guards; see c2-surface.ts header). Each test names the red it guards.

import { describe, expect, it } from 'vitest'
import { traceC2 } from './c2-trace'
import { SURFACE_SIZE, c2ScenarioById, type Arg } from './c2-surface'
import type { Manifest } from '../berry/catalog'
import { lint as lintFn } from '../berry/lint'

// Minimal manifest carrying net_clear as severing — the static half of the S8 probe.
const manifest: Manifest = {
  capabilities: [
    { name: 'connected', params: [], ret: 'bool', class: 'query', doc: 'Link up?' },
    { name: 'net_clear', params: [], ret: 'nil', class: 'command', risk: 'severing', doc: 'Clear the net cache.' },
  ],
  builtins: ['if', 'end', 'not', 'print'],
}

const val = (a: Arg): unknown => (a.kind === 'nil' ? null : (a as { value: unknown }).value)

describe('C2 surface completeness', () => {
  // Guards the 13-only regression (§2.3): a cmd-only stub set would run valid store/dev code into
  // "undefined". The surface must be the full berry_c2 count.
  it('stubs the full 36-function berry_c2 surface (cmd 13 + store 6 + rtc 4 + dev 13)', () => {
    expect(SURFACE_SIZE).toBe(36)
  })
})

describe('§7 probe (a) — 36-surface KV consistency (no "undefined")', () => {
  it('store_set then store_get returns the stored value, not nil', () => {
    const { trace } = traceC2('store_set("k","v")\nstore_get("k")')
    expect(trace.map((e) => e.fn)).toEqual(['store_set', 'store_get'])
    expect(trace[0].ok).toBe(true)
    expect(val(trace[0].ret)).toBe(true) // store_set → true
    expect(trace[1].ret.kind).toBe('string')
    expect(val(trace[1].ret)).toBe('v') // store_get → "v", NOT nil/undefined
  })

  it('a get for a never-set key is nil (but a clean nil, not a crash)', () => {
    const { trace } = traceC2('store_get("absent")')
    expect(trace[0].ret.kind).toBe('nil')
  })
})

describe('§7 probe (b) — firmware type strictness mirrored', () => {
  it('nvs_set with an int third arg is a rejected call (ok:false), like l_nvs_set silent-false', () => {
    const { trace } = traceC2('nvs_set("picpak","fmode",1)')
    expect(trace).toHaveLength(1)
    expect(trace[0].fn).toBe('nvs_set')
    expect(trace[0].ok).toBe(false) // int arg → the guard rejects it
    expect(val(trace[0].ret)).toBe(false)
  })

  it('nvs_set with the correct STRING third arg succeeds (the fmode fix contrast)', () => {
    const { trace } = traceC2('nvs_set("picpak","fmode","1")')
    expect(trace[0].ok).toBe(true)
    expect(val(trace[0].ret)).toBe(true)
  })
})

describe('§7 probe (c) — S8: static beats trace', () => {
  const src = 'if !connected() net_clear() end'

  it('the trace under connected=true OMITS net_clear (branch not taken)', () => {
    const { trace } = traceC2(src, c2ScenarioById('online'))
    const fns = trace.map((e) => e.fn)
    expect(fns).toContain('connected') // the guard call ran
    expect(fns).not.toContain('net_clear') // the guarded body did not
  })

  it('but the STATIC calls() pass reports net_clear as severing regardless of the branch', () => {
    const findings = lintFn(src, manifest)
    const severing = findings.filter((f) => f.kind === 'severing').map((f) => f.message)
    expect(severing.some((m) => m.startsWith('net_clear()'))).toBe(true)
  })

  it('flipping the scenario to offline makes the SAME script trace net_clear (scenario drives the path)', () => {
    const { trace } = traceC2(src, c2ScenarioById('offline'))
    expect(trace.map((e) => e.fn)).toContain('net_clear')
  })
})

describe('robustness — total, never throws', () => {
  it('an unknown bare call is recorded (not thrown), mirroring the unknown static lint', () => {
    const { trace, unknown } = traceC2('frobnicate(1,2)\nreboot()')
    expect(unknown).toContain('frobnicate')
    expect(trace.map((e) => e.fn)).toEqual(['reboot']) // known call still traced
  })

  it('a var binding feeding a guard evaluates without crashing', () => {
    const { trace } = traceC2('var up = connected()\nif up refresh() end')
    expect(trace.map((e) => e.fn)).toEqual(['connected', 'refresh'])
  })
})
