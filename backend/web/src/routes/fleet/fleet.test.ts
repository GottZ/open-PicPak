// Fleet dashboard logic — F1 (telemetry-delta merge), F2 (health glyph/class + membership), F3 (sentinel
// n/a + the {@html} ban), F4 (assigned-vs-reported channel). Pure node tests; each names the red it guards.

import { describe, expect, it } from 'vitest'
import fleetDashSrc from './FleetDashboard.svelte?raw'
import deviceCardSrc from './DeviceCard.svelte?raw'
import {
  type FleetRow,
  type Health,
  type TelemetryEvent,
  applyTelemetry,
  applyMembership,
  noDataRow,
  healthGlyph,
  healthClass,
  noDataLabel,
  metricLabel,
  battLabel,
  channelDisplay,
  newestTime,
  ageLabel,
  sparklinePath,
  grafanaLink,
} from './fleet'

function row(p: Partial<FleetRow> & { serial: string }): FleetRow {
  return {
    label: null,
    assigned_channel: 'stable',
    reg_last_seen: null,
    bonded: false,
    c2_last_seen: null,
    has_data: true,
    time: '2026-06-30T12:00:00Z',
    batt_mv: 3990,
    batt_pct: 80,
    bad_boots: 0,
    boot_count: 10,
    reset_reason: 'poweron',
    usb: false,
    uptime_ms: 1000,
    running_ver: 'fw-1',
    reported_channel: 'stable',
    diag_ota_rr: null,
    health: 'OK',
    reasons: [],
    channel_mismatch: false,
    ...p,
  }
}

describe('applyTelemetry (F1 — per-row delta merge)', () => {
  it('updates ONLY the matching serial and leaves others untouched', () => {
    const rows = [row({ serial: 'A', batt_pct: 80 }), row({ serial: 'B', batt_pct: 50, health: 'OK' })]
    const ev: TelemetryEvent = {
      serial: 'B',
      time: '2026-06-30T13:00:00Z',
      batt_pct: 12,
      batt_mv: 3600,
      running_ver: 'fw-2',
      health: 'LOW_BATT',
      last_seen: '2026-06-30T13:00:00Z',
      channel_mismatch: false,
    }
    const next = applyTelemetry(rows, ev)
    // the red: mutating in place / replacing the roster drops A's live state
    expect(next).not.toBe(rows)
    expect(next[0]).toBe(rows[0]) // A untouched (same reference)
    expect(next[1].batt_pct).toBe(12)
    expect(next[1].health).toBe('LOW_BATT')
    expect(next[1].running_ver).toBe('fw-2')
    expect(next[1].reg_last_seen).toBe('2026-06-30T13:00:00Z')
  })

  it('ignores a delta for an unknown serial (membership rides the devices channel)', () => {
    const rows = [row({ serial: 'A' })]
    const next = applyTelemetry(rows, { serial: 'Z', time: '', batt_pct: null, batt_mv: null, running_ver: '', health: 'OK', last_seen: null, channel_mismatch: false })
    expect(next).toBe(rows)
  })
})

describe('applyMembership (F2 — roster add/remove)', () => {
  it('adds a new serial as a NO_DATA placeholder, removes a vanished one', () => {
    let rows = [row({ serial: 'B' })]
    rows = applyMembership(rows, 'upsert', { serial: 'A', label: 'lab', channel: 'beta', last_seen: null, bonded: true })
    expect(rows.map((r) => r.serial)).toEqual(['A', 'B']) // serial-sorted
    expect(rows[0].health).toBe('NO_DATA')
    expect(rows[0].has_data).toBe(false)
    rows = applyMembership(rows, 'remove', { serial: 'B', label: null, channel: 'stable', last_seen: null, bonded: false })
    expect(rows.map((r) => r.serial)).toEqual(['A'])
  })

  it('an upsert for an existing serial keeps its telemetry-derived fields', () => {
    const rows = [row({ serial: 'A', running_ver: 'fw-9', health: 'OK', has_data: true })]
    const next = applyMembership(rows, 'upsert', { serial: 'A', label: 'new', channel: 'beta', last_seen: null, bonded: true })
    expect(next[0].label).toBe('new')
    expect(next[0].assigned_channel).toBe('beta')
    expect(next[0].running_ver).toBe('fw-9') // not clobbered
    expect(next[0].has_data).toBe(true)
  })
})

describe('healthGlyph / healthClass (F2 — every verdict incl NO_DATA)', () => {
  it('maps every Health to a glyph + class, including NO_DATA', () => {
    const all: Health[] = ['OK', 'LOW_BATT', 'BAD_BOOTS', 'BROWNOUT', 'OFFLINE_STALE', 'NO_DATA']
    for (const h of all) {
      expect(healthGlyph(h)).toBeTruthy()
      expect(['ok', 'warn', 'danger', 'muted']).toContain(healthClass(h))
    }
    expect(healthClass('OK')).toBe('ok')
    expect(healthClass('NO_DATA')).toBe('muted')
    expect(healthClass('BROWNOUT')).toBe('danger')
  })

  it('noDataLabel splits c2_alive from silent', () => {
    expect(noDataLabel(noDataRow({ serial: 'A', label: null, channel: 'stable', last_seen: null, bonded: false }))).toContain('silent')
    const alive = { ...noDataRow({ serial: 'A', label: null, channel: 'stable', last_seen: null, bonded: false }), reasons: ['c2_alive'] }
    expect(noDataLabel(alive)).toContain('C2-alive')
    expect(noDataLabel(row({ serial: 'A', has_data: true }))).toBe('') // data rows: no NO_DATA label
  })
})

describe('sentinel rendering (F3)', () => {
  it('metricLabel + battLabel render n/a for null, value otherwise', () => {
    expect(metricLabel(null)).toBe('n/a')
    expect(metricLabel(-32768)).toBe('-32768') // a real value passed through (the server already mapped sentinels)
    expect(metricLabel(42, '%')).toBe('42%')
    expect(battLabel(null, null)).toBe('n/a')
    expect(battLabel(80, 3990)).toBe('80% (3990mV)')
    expect(battLabel(null, 3990)).toBe('n/a (3990mV)')
  })

  it('neither page renders device-sourced strings via {@html} (D19.10)', () => {
    // match the DIRECTIVE form `{@html <expr>}` (whitespace/paren after @html), not a doc-comment `{@html}`
    expect(fleetDashSrc).not.toMatch(/\{@html[\s(]/)
    expect(deviceCardSrc).not.toMatch(/\{@html[\s(]/)
  })
})

describe('channelDisplay (F4 — mismatch flag)', () => {
  it('shows the reported channel ONLY on a server-flagged mismatch', () => {
    const mm = channelDisplay(row({ serial: 'A', assigned_channel: 'stable', reported_channel: 'beta', channel_mismatch: true }))
    expect(mm.mismatch).toBe(true)
    expect(mm.text).toContain('stable')
    expect(mm.text).toContain('beta')
    const ok = channelDisplay(row({ serial: 'A', assigned_channel: 'stable', reported_channel: 'stable', channel_mismatch: false }))
    expect(ok.mismatch).toBe(false)
    expect(ok.text).toBe('stable') // the red: surfacing the device-reported channel as authoritative
  })
})

describe('clocks, sparkline, grafana link', () => {
  it('newestTime picks the latest non-null timestamp', () => {
    expect(newestTime([null, '2026-06-30T10:00:00Z', '2026-06-30T12:00:00Z', null])).toBe('2026-06-30T12:00:00Z')
    expect(newestTime([null, null])).toBeNull()
  })

  it('ageLabel coarsens and tolerates null', () => {
    const now = Date.parse('2026-06-30T12:00:00Z')
    expect(ageLabel(null, now)).toBe('—')
    expect(ageLabel('2026-06-30T11:58:00Z', now)).toBe('2m ago')
    expect(ageLabel('2026-06-30T09:00:00Z', now)).toBe('3h ago')
  })

  it('sparklinePath skips nulls and needs >=2 real points', () => {
    expect(sparklinePath([null, 5], 100, 20)).toBe('') // one real point → no line
    const p = sparklinePath([0, 10, 5], 100, 20)
    expect(p.startsWith('M')).toBe(true)
    expect(p).toContain('L')
  })

  it('grafanaLink is empty when the base is unset (air-gap), else per-device (T13)', () => {
    expect(grafanaLink('', 'DEV1')).toBe('')
    expect(grafanaLink('https://grafana.example/d/x', 'DEV 1')).toBe('https://grafana.example/d/x?var-serial=DEV%201')
    expect(grafanaLink('https://grafana.example/d/x?orgId=1', 'DEV1')).toContain('&var-serial=DEV1')
  })
})
