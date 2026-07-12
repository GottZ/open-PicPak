// W3 gate (design 02-settings-spa §7-W3): the empty-env -> not_set classification.
// Pure node, no DOM — Muster lib/settings/secrets.test.ts. N2 negative probe
// (documented in the B3 report): a naive passthrough (`notSet: false` always,
// value rendered as-is) makes the first assertion below fail red — an unset env
// would then render as a blank cell instead of the not_set text.

import { describe, expect, it } from 'vitest'
import { diagRows } from './diagnostics'
import type { ConfigResponse } from '../api/types'

const cfg = (grafana: string, webhook: string): ConfigResponse => ({
  success: true,
  grafana_base_url: grafana,
  webhook_base_url: webhook,
})

describe('diagRows — empty env classifies as notSet, never a blank value (§7-W3, N2)', () => {
  it('flags an empty grafana_base_url as notSet', () => {
    const rows = diagRows(cfg('', 'https://hook.example/x'))
    const grafana = rows.find((r) => r.labelKey === 'settings.diag.field.grafana_base_url')
    expect(grafana?.notSet).toBe(true)
    expect(grafana?.value).toBe('') // the raw value stays '', the component renders not_set off notSet
  })

  it('flags an empty webhook_base_url as notSet', () => {
    const rows = diagRows(cfg('https://grafana.example', ''))
    const webhook = rows.find((r) => r.labelKey === 'settings.diag.field.webhook_base_url')
    expect(webhook?.notSet).toBe(true)
  })

  it('a non-empty value is never flagged notSet', () => {
    const rows = diagRows(cfg('https://grafana.example', 'https://hook.example/x'))
    for (const row of rows) expect(row.notSet).toBe(false)
  })

  it('covers exactly the two /api/config fields, in a stable order', () => {
    const rows = diagRows(cfg('a', 'b'))
    expect(rows.map((r) => r.labelKey)).toEqual([
      'settings.diag.field.grafana_base_url',
      'settings.diag.field.webhook_base_url',
    ])
  })
})
