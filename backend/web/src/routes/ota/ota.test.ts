// W1 — /ota Stored-XSS invariant (design 01-ota-spa §5 B6, §7-W1). `version` is a
// free TEXT field with no charset CHECK (0001_init.up.sql:20-21); every
// server-delivered string (version, sha256, …) must render as a text node
// ({…}), never {@html}. The AST gate (scripts/lint-no-html.ts) already covers
// every .svelte file structurally; this assertion pins the invariant locally at
// the OTA panel sources, mirroring fleet.test.ts:136-139.

import { describe, expect, it } from 'vitest'
import otaHomeSrc from './OtaHome.svelte?raw'
import firmwarePanelSrc from './FirmwarePanel.svelte?raw'
import channelPanelSrc from './ChannelPanel.svelte?raw'

describe('OTA panels never render device/operator-sourced strings via {@html} (B6)', () => {
  it('OtaHome, FirmwarePanel and ChannelPanel carry no {@html} directive', () => {
    // match the DIRECTIVE form `{@html <expr>}` (whitespace/paren after @html), not a doc-comment `{@html}`
    expect(otaHomeSrc).not.toMatch(/\{@html[\s(]/)
    expect(firmwarePanelSrc).not.toMatch(/\{@html[\s(]/)
    expect(channelPanelSrc).not.toMatch(/\{@html[\s(]/)
  })
})

// W3 — ChannelPanel Nicht-Admin-Negativ-Probe (design 01-ota-spa §7-W3): "als
// Nicht-Admin ist der Set-Button disabled mit Reason-Title (affordance) — erst
// ohne mutationAffordance rot (Button aktiv), dann grün." No component-render
// harness (@testing-library/svelte) exists in this repo (grep over src turns up
// none); every other panel pins its structural invariants via source-string
// assertions (the B6 block above), so this probe follows the same convention:
// it greps the ACTUAL wired-up JSX/attribute strings rather than re-describing
// them, so a regression (e.g. someone dropping the affordance off the button,
// or the handler guard) turns the test red.
// A6 — live reload-hint subscription (design 01-ota-spa §8-OQ2 (b), E2). Same
// source-assertion convention as B6 above (no component-render harness in this
// repo): both panels open an EventsClient with an onOta handler that reloads
// the matching Resource, and close the stream on destroy — the in-shell tab
// swap unmounts the panel, so a missing close would leak one SSE connection
// per tab switch against the hub's maxConn cap.
describe('OTA panels subscribe to the ota reload-hint (E2/A6)', () => {
  it('FirmwarePanel reloads its firmware Resource on a firmware hint and closes on destroy', () => {
    expect(firmwarePanelSrc).toMatch(/onOta:/)
    expect(firmwarePanelSrc).toMatch(/hint\.kind === 'firmware'\) void firmware\.reload\(\)/)
    expect(firmwarePanelSrc).toMatch(/onDestroy\(\(\) => \{\s*events\?\.close\(\)/)
  })

  it('ChannelPanel reloads channels + the version inventory on their hints and closes on destroy', () => {
    expect(channelPanelSrc).toMatch(/hint\.kind === 'channel'\) void channels\.reload\(\)/)
    expect(channelPanelSrc).toMatch(/hint\.kind === 'firmware'\) void firmware\.reload\(\)/)
    expect(channelPanelSrc).toMatch(/onDestroy\(\(\) => \{\s*events\?\.close\(\)/)
  })
})

describe('ChannelPanel Set-Default is admin-gated (§5 B1)', () => {
  it('the Set button is wired to mutationAffordance (disabled/title/aria-disabled)', () => {
    // the button element between `{m['ota.channel.set']()}` and its opening tag
    const setButton = channelPanelSrc.slice(
      channelPanelSrc.indexOf('onclick={() => setDefault('),
      channelPanelSrc.indexOf("{m['ota.channel.set']()}"),
    )
    expect(setButton).toMatch(/disabled=\{affordance\.disabled/)
    expect(setButton).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
    expect(setButton).toMatch(/aria-disabled=\{affordance\['aria-disabled'\]\}/)
  })

  it('setDefault() returns before mutating when session.is_admin is false (handler guard, §5 B1)', () => {
    const fn = channelPanelSrc.slice(
      channelPanelSrc.indexOf('async function setDefault'),
      channelPanelSrc.indexOf('async function setDefault') + 400,
    )
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })
})
