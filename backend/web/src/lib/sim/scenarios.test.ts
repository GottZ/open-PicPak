import { describe, it, expect } from 'vitest'
import { SCENARIOS, DEFAULT_SCENARIO_ID, scenarioById, devJson } from './scenarios'

describe('sim scenarios', () => {
  it('has the default id present and first (the sim_main.c defaults)', () => {
    expect(SCENARIOS[0].id).toBe(DEFAULT_SCENARIO_ID)
    expect(scenarioById(DEFAULT_SCENARIO_ID).dev).toEqual({ batt_mv: 3900, batt_pct: 70, uptime_ms: 1234 })
  })

  it('every scenario id is unique', () => {
    const ids = SCENARIOS.map((s) => s.id)
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('scenarioById falls back to the default for an unknown id (never throws)', () => {
    expect(scenarioById('does-not-exist')).toBe(SCENARIOS[0])
  })

  // §7 probe (c), pure half: the "battery almost empty" preset MUST differ from the default at the input
  // layer — otherwise the sliders could never move the frame. The end-to-end frame diff needs the WASM VM
  // (manual: build sim, pick "low", observe the battery text/colour change).
  it('the battery-low preset produces different device JSON than the default', () => {
    const normal = devJson(scenarioById('normal').dev)
    const low = devJson(scenarioById('low').dev)
    expect(low).not.toBe(normal)
    expect(scenarioById('low').dev.batt_pct).toBeLessThan(scenarioById('normal').dev.batt_pct)
  })

  it('devJson emits exactly the three wrapper-recognised integer fields', () => {
    const parsed = JSON.parse(devJson({ batt_mv: 3300, batt_pct: 8, uptime_ms: 240000 }))
    expect(parsed).toEqual({ batt_mv: 3300, batt_pct: 8, uptime_ms: 240000 })
    expect(Object.keys(parsed).sort()).toEqual(['batt_mv', 'batt_pct', 'uptime_ms'])
  })
})
