// effect-text — the Berry-call → human-effect translation table (design §4.3). Pure node tests: this
// module names an i18n key + params (the localized text lives in messages/{en,de}.json), so the assertions
// are on the descriptor, not on rendered German. Each names the red it guards.

import { describe, expect, it } from 'vitest'
import { describeEffect } from './effect-text'
import { str, int, type Arg } from './c2-surface'
import type { Manifest } from '../berry/catalog'

const manifest: Manifest = {
  capabilities: [
    { name: 'set_wifi', params: [], ret: 'bool', class: 'command', risk: 'severing', doc: 'Replace Wi-Fi creds.' },
    { name: 'store_set', params: [], ret: 'bool', class: 'store', doc: 'Persist a value in the LittleFS KV.' },
  ],
  builtins: [],
}

describe('effect-text curated table', () => {
  it('translates a command call to its curated key + params, carrying the severing flag', () => {
    const d = describeEffect('set_wifi', [str('home'), str('secret')] as Arg[], manifest)
    expect(d.source).toBe('curated')
    if (d.source !== 'curated') throw new Error('unreachable')
    expect(d.key).toBe('sim.effect.set_wifi')
    expect(d.params).toEqual({ ssid: 'home' })
    expect(d.severing).toBe(true)
  })

  it('works with NO args (gallery card path) — the param falls back to a placeholder, no crash', () => {
    const d = describeEffect('set_wifi')
    expect(d.source).toBe('curated')
    if (d.source !== 'curated') throw new Error('unreachable')
    expect(d.params.ssid).toBe('…')
  })

  it('renders a non-string arg via its Berry form (int stays typed, not stringified silently)', () => {
    const d = describeEffect('tx_power', [int(20)] as Arg[])
    if (d.source !== 'curated') throw new Error('unreachable')
    expect(d.params.dbm).toBe('20')
  })
})

describe('effect-text fallbacks', () => {
  it('falls back to the catalog doc for a non-curated capability (store/rtc/dev)', () => {
    const d = describeEffect('store_set', [str('k'), str('v')] as Arg[], manifest)
    expect(d.source).toBe('doc')
    if (d.source !== 'doc') throw new Error('unreachable')
    expect(d.text).toBe('Persist a value in the LittleFS KV.')
  })

  // §7 probe (d): an unknown function must yield raw text, never throw.
  it('an unknown function returns neutral raw text and never throws', () => {
    const d = describeEffect('frobnicate', [str('x')] as Arg[], manifest)
    expect(d.source).toBe('raw')
    if (d.source !== 'raw') throw new Error('unreachable')
    expect(d.text).toBe('frobnicate()')
    expect(d.severing).toBe(false)
  })

  it('unknown with no manifest at all is still raw, not a crash', () => {
    expect(describeEffect('whatever').source).toBe('raw')
  })
})
