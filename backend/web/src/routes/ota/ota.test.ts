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
import rolloutPanelSrc from './RolloutPanel.svelte?raw'
import { resolveSourceLabel } from '../../lib/ota/rollouts'
import type { Source } from '../../lib/ota/types'
import { m } from '../../paraglide/messages.js'

describe('OTA panels never render device/operator-sourced strings via {@html} (B6)', () => {
  it('OtaHome, FirmwarePanel, ChannelPanel and RolloutPanel carry no {@html} directive', () => {
    // match the DIRECTIVE form `{@html <expr>}` (whitespace/paren after @html), not a doc-comment `{@html}`
    expect(otaHomeSrc).not.toMatch(/\{@html[\s(]/)
    expect(firmwarePanelSrc).not.toMatch(/\{@html[\s(]/)
    expect(channelPanelSrc).not.toMatch(/\{@html[\s(]/)
    expect(rolloutPanelSrc).not.toMatch(/\{@html[\s(]/)
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

// W4 — RolloutPanel (design 01-ota-spa §4.4/§7 W4). Same source-assertion
// convention as above (no component-render harness in this repo).
describe('RolloutPanel delete is two-step armed, not a single-click destroy (§4.4, MediaHome pattern, N1)', () => {
  // N1 — the load-bearing negative probe: doDelete() MUST arm-and-return on an
  // unarmed row (armed !== id) BEFORE it ever reaches the DELETE fetch call.
  // Structurally pinned here: a single click on a fresh row can only set
  // `armed`, never call apiFetch(..., {method:'DELETE'}) in the same
  // invocation — verified live (§ report): a version of doDelete() with the
  // `if (armed !== id) { armed = id; return }` guard removed makes this
  // assertion fail red, because the DELETE fetch line then appears BEFORE any
  // armed-check in the function body.
  it('doDelete arms on the first click and returns before the DELETE fetch call', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function doDelete')
    const fn = rolloutPanelSrc.slice(fnStart, rolloutPanelSrc.indexOf('\n  }\n', fnStart))
    const armIdx = fn.search(/if \(armed !== id\) \{\s*armed = id/)
    const deleteFetchIdx = fn.indexOf("method: 'DELETE'")
    expect(armIdx).toBeGreaterThan(-1)
    expect(deleteFetchIdx).toBeGreaterThan(-1)
    expect(armIdx).toBeLessThan(deleteFetchIdx) // the arm-and-return guard sits BEFORE the DELETE call
  })

  it('a second click on the SAME armed row is required to reach the DELETE fetch (armed !== id gate)', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function doDelete')
    const fn = rolloutPanelSrc.slice(fnStart, rolloutPanelSrc.indexOf('\n  }\n', fnStart))
    // the guard must reference the SAME `id` the DELETE call targets, not a
    // different identifier that would let the check drift from the mutation.
    expect(fn).toMatch(/if \(armed !== id\)/)
    expect(fn).toMatch(/`\/api\/rollouts\/\$\{id\}`, \{ method: 'DELETE' \}/)
  })
})

describe('RolloutPanel mutations are admin-gated (§5 B1)', () => {
  it('submitUpsert() returns before POSTing when session.is_admin is false', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function submitUpsert')
    const fn = rolloutPanelSrc.slice(fnStart, fnStart + 400)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('setRolloutState() returns before PATCHing when session.is_admin is false', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function setRolloutState')
    const fn = rolloutPanelSrc.slice(fnStart, fnStart + 400)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('doDelete() returns before mutating when session.is_admin is false', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function doDelete')
    const fn = rolloutPanelSrc.slice(fnStart, fnStart + 400)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('the Create/Upsert button is wired to mutationAffordance (disabled/title/aria-disabled)', () => {
    const btn = rolloutPanelSrc.slice(
      rolloutPanelSrc.indexOf('type="submit"'),
      rolloutPanelSrc.indexOf("{m['ota.rollout.create']()}"),
    )
    expect(btn).toMatch(/disabled=\{!canSubmit\}/)
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
    expect(btn).toMatch(/aria-disabled=\{affordance\['aria-disabled'\]\}/)
  })
})

// A6 — live reload-hint subscription (design 01-ota-spa §8-OQ2 (b), E2), same
// convention as the FirmwarePanel/ChannelPanel pins above.
describe('RolloutPanel subscribes to the ota reload-hint (E2/A6)', () => {
  it('reloads rollouts/channels/firmware on their respective hints and closes on destroy', () => {
    expect(rolloutPanelSrc).toMatch(/onOta:/)
    expect(rolloutPanelSrc).toMatch(/hint\.kind === 'rollout'\) void rollouts\.reload\(\)/)
    expect(rolloutPanelSrc).toMatch(/hint\.kind === 'channel'\) void channels\.reload\(\)/)
    expect(rolloutPanelSrc).toMatch(/hint\.kind === 'firmware'\) void firmware\.reload\(\)/)
    expect(rolloutPanelSrc).toMatch(/onDestroy\(\(\) => \{\s*events\?\.close\(\)/)
  })
})

// W5 — Resolve-Vorschau (design 01-ota-spa §3/§4.4/§4.6 Bindestrich-Falle, N1).
describe('resolveSourceLabel (§3 Bindestrich-Falle)', () => {
  it('maps all 4 resolver sources to their i18n label, including the hyphen/underscore case', () => {
    expect(resolveSourceLabel('serial')).toBe(m['ota.resolve.source.serial']())
    expect(resolveSourceLabel('fleet')).toBe(m['ota.resolve.source.fleet']())
    expect(resolveSourceLabel('channel-default')).toBe(m['ota.resolve.source.channel_default']())
    expect(resolveSourceLabel('none')).toBe(m['ota.resolve.source.none']())
  })

  // N1 — the load-bearing negative probe (§3): the resolver's wire value for
  // this source is 'channel-default' WITH A HYPHEN (internal/rollout/types.go:15,
  // lib/ota/types.ts Source), but the message key is
  // 'ota.resolve.source.channel_default' WITH AN UNDERSCORE. A naive
  // `m['ota.resolve.source.' + source]()` (string concatenation, exactly what
  // §3 warns against) would look up the key below — which literally does not
  // exist in the compiled message catalog `m`. Paraglide does not throw for an
  // unknown property access at the type level (a raw string index would just
  // be `undefined` at runtime, not silently "work"); the point pinned here is
  // that concatenation targets a WRONG, non-existent key, while
  // resolveSourceLabel's explicit Record targets the real one. Verified live:
  // reverting resolveSourceLabel to `m['ota.resolve.source.' + source as any]`
  // makes the source-code assertion below fail red (the src still concatenates),
  // and TypeScript itself refuses `m[...]` on a non-literal key — the Record
  // is the only construction that both type-checks and resolves correctly.
  it('the naive-concatenation key for channel-default does not exist in the message catalog', () => {
    const source: Source = 'channel-default'
    const naiveKey = 'ota.resolve.source.' + source
    expect(naiveKey).toBe('ota.resolve.source.channel-default') // the wrong, hyphenated key a concatenation produces
    expect(naiveKey).not.toBe('ota.resolve.source.channel_default') // the real, underscored key
    expect(Object.keys(m)).not.toContain(naiveKey)
    expect(Object.keys(m)).toContain('ota.resolve.source.channel_default')
  })

  it('RolloutPanel resolves the source label via the Record helper, not string concatenation', () => {
    expect(rolloutPanelSrc).toMatch(/resolveSourceLabel\(/)
    expect(rolloutPanelSrc).not.toMatch(/'ota\.resolve\.source\.'\s*\+/)
    expect(rolloutPanelSrc).not.toMatch(/`ota\.resolve\.source\.\$\{/)
  })
})

// N2 — source='none' (fail-open, read.go:18-24) renders through the SAME ready
// path as every other source; it is not an error state (§4.4/§7-W5).
describe('RolloutPanel resolve treats source="none" as a normal result, not an error (N2)', () => {
  it('resolveDevice() does not special-case source===\'none\' as an error/toast', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function resolveDevice')
    const fn = rolloutPanelSrc.slice(fnStart, rolloutPanelSrc.indexOf('\n  }\n', fnStart))
    expect(fn).not.toMatch(/source\s*===\s*['"]none['"]/)
    expect(fn).not.toMatch(/notify\.error/)
  })

  it('the resolved-result markup renders resolveSourceLabel unconditionally (no none-branch around it)', () => {
    const resultStart = rolloutPanelSrc.indexOf('class="resolve-result"')
    const resultBlock = rolloutPanelSrc.slice(resultStart, resultStart + 400)
    expect(resultBlock).toMatch(/resolveSourceLabel\(resolved\.resolved\.source\)/)
    expect(resultBlock).not.toMatch(/'none'/)
  })
})

// E4 (§8-OQ4 Abweichung "auch wechseln") — the channel-move mutation is
// admin-gated the same way every other RolloutPanel mutation is (§5 B1).
describe('RolloutPanel channel-move (E4) is admin-gated and PATCHes devices.go:88-117', () => {
  it('submitChannelChange() returns before PATCHing when session.is_admin is false', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function submitChannelChange')
    const fn = rolloutPanelSrc.slice(fnStart, fnStart + 400)
    expect(fn).toMatch(/if \(!session\.is_admin[^)]*\)\s*return/)
  })

  it('PATCHes /api/devices/{serial} with a {channel} body, matching devices.go patch()', () => {
    const fnStart = rolloutPanelSrc.indexOf('async function submitChannelChange')
    const fn = rolloutPanelSrc.slice(fnStart, fnStart + 600)
    expect(fn).toMatch(/`\/api\/devices\/\$\{resolveSerial\}`, \{/)
    expect(fn).toMatch(/method: 'PATCH'/)
    expect(fn).toMatch(/channel: pickedChannel/)
  })

  it('the Wechseln button is wired to mutationAffordance (disabled/title/aria-disabled)', () => {
    const btnStart = rolloutPanelSrc.indexOf('onclick={submitChannelChange}')
    const btn = rolloutPanelSrc.slice(btnStart, rolloutPanelSrc.indexOf("{m['ota.resolve.change']()}"))
    expect(btn).toMatch(/disabled=\{affordance\.disabled/)
    expect(btn).toMatch(/title=\{affordance\.disabled \? affordance\.title : ''\}/)
    expect(btn).toMatch(/aria-disabled=\{affordance\['aria-disabled'\]\}/)
  })
})
