// Static snippet summaries (design/33 §4.5b/§5 S8, W-A33.5b). Pure node tests. The S8 probe (c) is the
// load-bearing one: a severing call hidden behind a branch must still be reported by the STATIC pass.

import { describe, expect, it } from 'vitest'
import { calledCapabilities, effectSummary, severingCalls } from './summary'
import type { Manifest } from '../berry/catalog'

const manifest: Manifest = {
  capabilities: [
    { name: 'set_url', params: [], ret: 'bool', class: 'command', risk: 'severing', doc: 'Set the C2 poll URL.' },
    { name: 'net_clear', params: [], ret: 'bool', class: 'command', risk: 'severing', doc: 'Clear the cached net config.' },
    { name: 'refresh', params: [], ret: 'bool', class: 'command', doc: 'Force a frame refresh.' },
    { name: 'store_set', params: [], ret: 'bool', class: 'store', doc: 'Persist a value in the KV.' },
  ],
  builtins: ['print', 'if', 'end'],
}

describe('calledCapabilities', () => {
  it('extracts distinct catalog calls in order, skipping builtins and member calls', () => {
    const src = `refresh()\nprint("hi")\nfoo.bar()\nstore_set("k","v")\nrefresh()`
    expect(calledCapabilities(src, manifest)).toEqual(['refresh', 'store_set'])
  })

  it('ignores calls inside strings and comments (stripped first)', () => {
    const src = `# net_clear() in a comment\nx = "set_url(1)"\nrefresh()`
    expect(calledCapabilities(src, manifest)).toEqual(['refresh'])
  })
})

describe('effectSummary', () => {
  it('describes each call — a catalog store cap falls back to its doc, no crash on args', () => {
    const sum = effectSummary(`store_set("k","v")`, manifest)
    expect(sum).toHaveLength(1)
    expect(sum[0].source).toBe('doc')
  })
})

describe('severingCalls — S8 probe (c)', () => {
  // The hidden-severing fixture: net_clear sits behind a condition that a sim/trace under benign device
  // state would never enter, but the STATIC scan is branch-independent, so the fleet-'*' confirm still
  // warns. Red: sourcing the warning from a dynamic trace (which would not enter the branch) drops it.
  it('reports a severing call buried in a never-taken branch (static, not trace)', () => {
    const src = `refresh()\nif battery_low_never_true\n  net_clear()\nend`
    expect(severingCalls(src, manifest)).toEqual(['net_clear'])
  })

  it('a snippet with no severing call reports none', () => {
    expect(severingCalls(`refresh()\nstore_set("k","v")`, manifest)).toEqual([])
  })
})
