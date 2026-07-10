/// <reference types="node" />
// Node builtins are used only for the firmware drift-guard below (this test runs in vitest's node env);
// the reference pulls @types/node for this file alone (the app tsconfig omits "node" from its types).
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, it, expect } from 'vitest'
import { SIM_EXAMPLES, STARTER_SOURCE, exampleById } from './examples'

// firmware/test/*.be relative to this test file (backend/web/src/lib/sim/ → repo firmware/test/).
const fwPath = (name: string) =>
  fileURLToPath(new URL(`../../../../../firmware/test/${name}`, import.meta.url))

describe('sim examples', () => {
  it('exposes starter + the two firmware mirrors, ids unique, starter first', () => {
    expect(SIM_EXAMPLES[0].id).toBe('starter')
    expect(SIM_EXAMPLES.map((e) => e.id)).toEqual(['starter', 'render16', 'render_qr'])
    const ids = SIM_EXAMPLES.map((e) => e.id)
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('the starter draws the stub knobs (so presets are visible in the frame)', () => {
    expect(STARTER_SOURCE).toContain('dev_batt_pct()')
    expect(STARTER_SOURCE).toContain('dev_batt_mv()')
    expect(STARTER_SOURCE).toContain('dev_uptime_ms()')
  })

  // §7 probe (d), pure half: the render16 example the dropdown loads IS the firmware fixture, byte-for-byte
  // — a drifting firmware script turns this red rather than shipping a stale "mirror". The end-to-end
  // "loads into the editor" step is the panel's setDoc(exampleById('render16').source); manual with WASM.
  it('render16 mirrors firmware/test/render16.be byte-for-byte', () => {
    const fw = readFileSync(fwPath('render16.be'), 'utf8')
    expect(exampleById('render16')?.source).toBe(fw)
  })

  it('render_qr mirrors firmware/test/render_qr.be byte-for-byte', () => {
    const fw = readFileSync(fwPath('render_qr.be'), 'utf8')
    expect(exampleById('render_qr')?.source).toBe(fw)
  })

  it('exampleById returns null for an unknown id', () => {
    expect(exampleById('nope')).toBeNull()
  })
})
