// Scenario presets for the render-profile simulator (design/33 §4.3). The mid-tier operator picks a
// named device state instead of hand-editing raw JSON; a "raw values" panel (SimulatorPanel.svelte)
// exposes the same knobs for power users. CM6-free and pure so the presets — and the invariant that
// distinct presets produce distinct device JSON (the "the sliders demonstrably move the frame" probe,
// §7 probe c) — are node-testable without the WASM VM.
//
// SCOPE (honest, §4.3 vs the built W-A33.2 wrapper): the render-profile wrapper (firmware/sim/sim_main.c)
// only implements sim_set_dev, whose recognised knobs are batt_mv / batt_pct / uptime_ms. The design's
// "Nacht 03:00" (RTC) and "kein NVS provisioniert" (NVS) presets are NOT offered here: sim_set_rtc /
// sim_set_nvs were not built in W-A33.2 (no RTC/NVS setter in sim_main.c). Adding those presets would
// silently no-op — worse than omitting them. They return once the C2 profile (W-A33.4) lands the wider
// stub surface. The presets below map one-to-one onto real, wired knobs.

/** The stub device knobs sim_set_dev(json) recognises (firmware/sim/sim_main.c json_int fields). */
export interface DevState {
  batt_mv: number
  batt_pct: number
  uptime_ms: number
}

export interface Scenario {
  /** Stable id → i18n label key `sim.scenario.<id>` and the <select> value. */
  id: string
  dev: DevState
}

// Ordered for the dropdown; `normal` mirrors the sim_main.c defaults and is the initial selection.
export const SCENARIOS: readonly Scenario[] = [
  { id: 'normal', dev: { batt_mv: 3900, batt_pct: 70, uptime_ms: 1234 } },
  { id: 'fresh', dev: { batt_mv: 4100, batt_pct: 95, uptime_ms: 1200 } },
  { id: 'low', dev: { batt_mv: 3300, batt_pct: 8, uptime_ms: 240000 } },
  { id: 'aged', dev: { batt_mv: 3600, batt_pct: 35, uptime_ms: 3600000 } },
] as const

export const DEFAULT_SCENARIO_ID = 'normal'

/** The scenario for an id, or the default when the id is unknown (never throws). */
export function scenarioById(id: string): Scenario {
  return SCENARIOS.find((s) => s.id === id) ?? SCENARIOS[0]
}

/**
 * The sim_set_dev(json) payload for a device state. Only the three recognised integer fields are emitted
 * (the wrapper's json_int ignores anything else) — keeping the wire honest about what the render profile
 * actually reads.
 */
export function devJson(dev: DevState): string {
  return JSON.stringify({ batt_mv: dev.batt_mv, batt_pct: dev.batt_pct, uptime_ms: dev.uptime_ms })
}
