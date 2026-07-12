// W1 — /ota Stored-XSS invariant (design 01-ota-spa §5 B6, §7-W1). `version` is a
// free TEXT field with no charset CHECK (0001_init.up.sql:20-21); every
// server-delivered string (version, sha256, …) must render as a text node
// ({…}), never {@html}. The AST gate (scripts/lint-no-html.ts) already covers
// every .svelte file structurally; this assertion pins the invariant locally at
// the OTA panel sources, mirroring fleet.test.ts:136-139.

import { describe, expect, it } from 'vitest'
import otaHomeSrc from './OtaHome.svelte?raw'
import firmwarePanelSrc from './FirmwarePanel.svelte?raw'

describe('OTA panels never render device/operator-sourced strings via {@html} (B6)', () => {
  it('OtaHome and FirmwarePanel carry no {@html} directive', () => {
    // match the DIRECTIVE form `{@html <expr>}` (whitespace/paren after @html), not a doc-comment `{@html}`
    expect(otaHomeSrc).not.toMatch(/\{@html[\s(]/)
    expect(firmwarePanelSrc).not.toMatch(/\{@html[\s(]/)
  })
})
