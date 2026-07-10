import { describe, it, expect } from 'vitest'
import { pinLine, type SimManifest } from './manifest'

const FIXTURE: SimManifest = {
  berry_pin: 'bd9c93b65dfadddc27e3203fc04e60e986a0fa5f',
  berry_conf_base_sha256: 'x',
  sim_conf_patch_sha256: 'y',
  fb_c_sha256: 'z',
  emsdk_version: '4.0.16',
  expected_wasm_br_bytes: 81478,
}

describe('sim manifest', () => {
  it('formats a short human pin line', () => {
    expect(pinLine(FIXTURE)).toBe('berry bd9c93b · emsdk 4.0.16 · 79.6 KB')
  })
})
