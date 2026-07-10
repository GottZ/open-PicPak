// "Load example" sources for the render-profile simulator (design/33 §4.3 — the directive's "neben den
// Beispielen"). Pure and CM6-free so the catalogue is node-testable; the panel loads a chosen source into
// its own editor (SimulatorPanel.svelte).
//
// The `render16` / `render_qr` entries MIRROR the firmware host-render test fixtures
// (firmware/test/render16.be, firmware/test/render_qr.be) — the same scripts host_render.c renders to
// byte-identical frames (§2.1). They are embedded (not fetched) so the simulator has zero backend
// dependency; examples.test.ts pins each mirror byte-for-byte against its firmware source, so any drift
// in the firmware fixture turns the test red (a phantom "mirror" cannot rot silently). The design's
// "builtin-render-templates from the A30 catalogue" as a second source is a §9 A30 merge point (the
// catalogue-mirror of these fixtures) — not built here; the firmware fixtures are the honest W-A33.3 set.

export interface SimExample {
  /** Stable id → i18n label key `sim.example.<id>` and the <select> value. */
  id: string
  source: string
}

// The default seed for a fresh editor: a minimal render script that DRAWS the three stub knobs, so the
// scenario presets (scenarios.ts) are visibly reflected in the frame — the concrete substance behind
// "the sliders demonstrably move the frame" (§7 probe c). Kept short and self-explanatory for the
// mid-tier operator opening the panel for the first time.
export const STARTER_SOURCE = `# Berry render script. Draws the stub device state, so the scenario
# presets below visibly change the frame. Edit freely, then press Run.
fill(1)                                        # white background
text16(14, 16, "PicPak render sim", 0)
text16(14, 46, "Battery: " + str(dev_batt_pct()) + " %", 0)
text16(14, 76, str(dev_batt_mv()) + " mV", 3)  # red
text(14, 110, "uptime " + str(dev_uptime_ms()) + " ms", 0)
`

// Mirror of firmware/test/render16.be (pinned byte-for-byte by examples.test.ts).
const RENDER16_SOURCE = `# host-PNG test fixture: exercise the 16px font + the text/text16 nil-safety hardening.
# Run via host/host_render against the live fb.c -> host/fb2png.py -> inspect the PNG.
fill(1)                                      # white background
text16(14, 16, "Hello 16px font", 0)         # 16px black
text16(14, 42, "0123456789 BWRY", 3)         # 16px red
text16(14, 68, "abcdefghijklm nop", 0)       # lowercase coverage
text(14, 100, "8px font for size compare", 0)
text16(14, 120, "scale x2:", 0)
text16(150, 116, "BIG", 0, 2)                # scaled 16px

# H4 negative probes: a non-string slot must be a clean no-op, never a crash.
text(14, 230, nil)                           # nil string  -> hardened text() returns nil
text16(14, 250, 42, 0)                       # int as string slot -> hardened text16() returns nil
text16(14, 272, "rendered past the nils OK", 0)   # proves execution continued
`

// Mirror of firmware/test/render_qr.be (pinned byte-for-byte by examples.test.ts).
const RENDER_QR_SOURCE = `# host-PNG test fixture for qr(): two real QR payloads + an overlong no-crash probe.
fill(1)
text16(8, 4, "WiFi join:", 0)
qr(8, 30, "WIFI:T:WPA;S:PicPak;P:hunter2;;", 3)        # Wi-Fi join QR (black on white, scale 3)
text16(230, 4, "Web UI:", 0)
qr(238, 30, "http://192.168.1.50", 3)                  # LAN URL QR

# negative probe: a payload too long for QR_MAXVER(10) -> qr() draws nothing (no partial, no crash)
var big = ""
for i : 0 .. 40
  big += "0123456789ABCDEFGHIJ"                         # 41*20 = 820 chars, well over V10
end
qr(8, 236, big, 2)
text16(8, 244, "after overlong qr: OK", 0)             # proves execution continued past the failed qr
`

// Ordered for the dropdown; `starter` first (the initial editor seed).
export const SIM_EXAMPLES: readonly SimExample[] = [
  { id: 'starter', source: STARTER_SOURCE },
  { id: 'render16', source: RENDER16_SOURCE },
  { id: 'render_qr', source: RENDER_QR_SOURCE },
] as const

/** The example for an id, or null when unknown (the panel ignores an unknown selection). */
export function exampleById(id: string): SimExample | null {
  return SIM_EXAMPLES.find((e) => e.id === id) ?? null
}
