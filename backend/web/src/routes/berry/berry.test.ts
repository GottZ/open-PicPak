// Berry editor logic — T1 (catalog-bounded completion), T2 (forbidden-surface lint), T3 (balance lint),
// plus the severing mark + the unknown/did-you-mean path. Pure node tests (no CM6 import — completion.ts
// wraps the CM6 editor; the SURFACE lives in catalog.ts). Each names the red it guards.

import { describe, expect, it } from 'vitest'
import berryEditorSrc from './BerryEditor.svelte?raw'
import { type Manifest, completionOptions, signature } from '../../lib/berry/catalog'
import { lint, suggest } from '../../lib/berry/lint'

// A representative manifest mirroring the real be_regfunc surface (the parity-tested Go manifest). It
// carries the three Doc-13b-prose divergences' CORRECT names (device_sleep/wifi_add, no sleep/info/
// set_wifi_add) and the forbidden net/fb names — exactly what the linter + completion must respect.
const manifest: Manifest = {
  capabilities: [
    { name: 'set_url', params: [{ name: 'url', type: 'string' }], ret: 'bool', class: 'command', risk: 'severing', doc: 'Set the C2 URL.' },
    { name: 'set_wifi', params: [{ name: 'ssid', type: 'string' }, { name: 'pass', type: 'string' }], ret: 'bool', class: 'command', risk: 'severing', doc: 'Replace Wi-Fi creds.' },
    { name: 'wifi_add', params: [{ name: 'ssid', type: 'string' }, { name: 'pass', type: 'string' }, { name: 'prio', type: 'int', opt: true }], ret: 'bool', class: 'command', doc: 'Add a Wi-Fi network.' },
    { name: 'tx_power', params: [{ name: 'dbm', type: 'int', opt: true }], ret: 'int', class: 'command', doc: 'Get/set TX power.' },
    { name: 'connected', params: [], ret: 'bool', class: 'query', doc: 'Link up?' },
    { name: 'reboot', params: [], ret: 'nil', class: 'intent', doc: 'Reboot after the poll.' },
    { name: 'device_sleep', params: [{ name: 's', type: 'int', opt: true }], ret: 'nil', class: 'intent', doc: 'Deep-sleep s seconds.' },
    { name: 'store_set', params: [{ name: 'key', type: 'string' }, { name: 'val', type: 'any' }], ret: 'bool', class: 'store', doc: 'Persist a value.' },
    { name: 'dev_batt_pct', params: [], ret: 'int', class: 'dev', doc: 'Battery percent.' },
    { name: 'http_get', params: [], ret: '', class: 'forbidden', doc: 'Policy-phase (net, Wi-Fi up) capability — not in the C2 executor.' },
    { name: 'text', params: [], ret: '', class: 'forbidden', doc: 'Render-phase (fb drawing) capability — not in the C2 executor.' },
  ],
  builtins: ['print', 'json', 'if', 'for', 'end', 'import', 'str', 'int'],
}

const names = (m: Manifest) => completionOptions(m).map((o) => o.label)

describe('T1 — catalog-bounded completion', () => {
  it('offers the real be_regfunc names', () => {
    const offered = names(manifest)
    expect(offered).toContain('device_sleep')
    expect(offered).toContain('wifi_add')
    expect(offered).toContain('set_url')
    expect(offered).toContain('print') // builtins are offered too (no lint false-positive on them)
  })

  it('NEVER offers a forbidden name (render/policy surface)', () => {
    const offered = names(manifest)
    // red: a completion source that didn't filter class:'forbidden' would offer these → they compile
    // but fault at runtime while the cursor advances (§2.4/§2.5).
    expect(offered).not.toContain('http_get')
    expect(offered).not.toContain('text')
  })

  it('NEVER offers the Doc-13b-prose names that do not exist on-device', () => {
    const offered = names(manifest)
    // red: a catalog seeded from 13b prose would offer sleep/info/set_wifi_add — none are be_regfunc'd.
    expect(offered).not.toContain('sleep')
    expect(offered).not.toContain('info')
    expect(offered).not.toContain('set_wifi_add')
  })

  it('renders bracketed signatures for optional args', () => {
    expect(signature(manifest.capabilities.find((c) => c.name === 'device_sleep')!)).toBe('device_sleep([s])')
    expect(signature(manifest.capabilities.find((c) => c.name === 'wifi_add')!)).toBe('wifi_add(ssid, pass[, prio])')
  })
})

describe('T2 — forbidden-surface lint', () => {
  it('flags a forbidden call and names its real phase', () => {
    const net = lint('http_get("x")', manifest)
    const f = net.find((x) => x.kind === 'forbidden')
    expect(f).toBeTruthy()
    expect(f!.message).toContain('policy-phase')
    // red: no catalog check → the call passes lint, then faults silently on-device while cursor advances.

    const render = lint('text("hi")', manifest)
    expect(render.find((x) => x.kind === 'forbidden')!.message).toContain('render-phase')
  })
})

describe('T3 — balance lint', () => {
  it('flags a missing end', () => {
    const f = lint('if connected()\n  reboot()\n', manifest)
    expect(f.some((x) => x.kind === 'balance' && /missing 'end'/.test(x.message))).toBe(true)
    // red: a naive line-count linter misses the missing end.
  })

  it('passes a balanced if/…/end', () => {
    const f = lint('if connected()\n  reboot()\nend\n', manifest)
    expect(f.some((x) => x.kind === 'balance')).toBe(false)
  })

  it('flags an unclosed paren and an unclosed string', () => {
    expect(lint('reboot(\n', manifest).some((x) => x.kind === 'balance')).toBe(true)
    expect(lint('set_url("oops\n)', manifest).some((x) => x.kind === 'string')).toBe(true)
  })

  it('does NOT count keywords inside comments or strings', () => {
    // an `if` in a comment / string must not create a phantom open block.
    expect(lint('# if this were real it would need end\nreboot()\n', manifest).some((x) => x.kind === 'balance')).toBe(false)
    expect(lint('store_set("k", "if for def")\n', manifest).some((x) => x.kind === 'balance')).toBe(false)
  })
})

describe('severing mark + unknown/did-you-mean', () => {
  it('marks a severing capability call', () => {
    const f = lint('set_wifi("a", "b")', manifest).find((x) => x.kind === 'severing')
    expect(f).toBeTruthy()
    expect(f!.message).toContain('severing')
  })

  it('suggests device_sleep for the 13b-prose sleep()', () => {
    const f = lint('sleep(5)', manifest).find((x) => x.kind === 'unknown')
    expect(f).toBeTruthy()
    expect(f!.message).toContain('device_sleep')
  })

  it('suggest() resolves the known divergences and plain typos', () => {
    const cands = manifest.capabilities.filter((c) => c.class !== 'forbidden').map((c) => c.name)
    expect(suggest('sleep', cands)).toBe('device_sleep') // containment
    expect(suggest('rebot', cands)).toBe('reboot') // levenshtein ≤2
    expect(suggest('zzzzzz', cands)).toBeNull()
  })

  it('does not flag a user-defined function or a member call', () => {
    const f = lint('def helper()\n  reboot()\nend\nhelper()\n', manifest)
    expect(f.some((x) => x.kind === 'unknown')).toBe(false)
    // member calls (json.load) are not bare globals → not flagged unknown.
    expect(lint('import json\nvar c = json.load("{}")\n', manifest).some((x) => x.kind === 'unknown')).toBe(false)
  })
})

describe('D19.10 — {@html} ban on operator/device strings', () => {
  it('the editor renders all catalog/script text as text nodes', () => {
    expect(berryEditorSrc).not.toContain('{@html')
  })
})
