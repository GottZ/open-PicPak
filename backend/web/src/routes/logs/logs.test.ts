// Log viewer logic — F1 (marker synthesis), F2 (reconnect-backfill reconcile), F3 (device text stays
// inert data + the {@html} ban). Pure node tests; each names the red it guards.

import { describe, expect, it } from 'vitest'
import logViewerSrc from './LogViewer.svelte?raw'
import {
  type LogLine,
  type LogEvent,
  logEventToLines,
  reconcile,
  synthesizeView,
  keyOf,
} from './logs'

function line(p: Partial<LogLine> & { serial: string; seq: number | null; idx: number }): LogLine {
  return {
    time: '2026-01-01T00:00:00Z',
    boot_count: 1,
    source: 'telemetry',
    gap: false,
    suspect: false,
    text: 'x',
    ...p,
  }
}

describe('synthesizeView (F1 marker synthesis)', () => {
  it('a server gap=true line gets a gap divider even with NO boot_count change', () => {
    // the red: deriving dividers only from client boot-count deltas misses a server-flagged gap
    const v = synthesizeView([
      line({ serial: 'A', seq: 1, idx: 0, boot_count: 5, text: 'one' }),
      line({ serial: 'A', seq: 2, idx: 0, boot_count: 5, gap: true, text: 'two' }),
    ])
    const dividers = v.filter((i) => i.kind === 'divider')
    expect(dividers).toHaveLength(1)
    expect(dividers[0]).toMatchObject({ variant: 'gap' })
    const gi = v.findIndex((i) => i.kind === 'divider')
    expect(v[gi + 1]).toMatchObject({ kind: 'line', line: { text: 'two' } }) // divider precedes the line
  })

  it('a boot_count transition with no gap flag gets a boot divider', () => {
    const v = synthesizeView([
      line({ serial: 'A', seq: 1, idx: 0, boot_count: 5 }),
      line({ serial: 'A', seq: 2, idx: 0, boot_count: 6 }),
    ])
    expect(v.filter((i) => i.kind === 'divider' && i.variant === 'boot')).toHaveLength(1)
  })

  it('a suspect=true line gets a suspect divider', () => {
    const v = synthesizeView([line({ serial: 'A', seq: 1, idx: 0, suspect: true })])
    expect(v.filter((i) => i.kind === 'divider' && i.variant === 'suspect')).toHaveLength(1)
  })

  it('continuation tokens of one row produce no duplicate divider', () => {
    const v = synthesizeView([
      line({ serial: 'A', seq: 7, idx: 0, gap: true, text: 'a' }),
      line({ serial: 'A', seq: 7, idx: 1, gap: false, text: 'b' }),
    ])
    expect(v.filter((i) => i.kind === 'divider')).toHaveLength(1)
    expect(v.filter((i) => i.kind === 'line')).toHaveLength(2)
  })

  it('no divider precedes the very first line when it carries no flags', () => {
    const v = synthesizeView([line({ serial: 'A', seq: 1, idx: 0, boot_count: 3 })])
    expect(v[0]).toMatchObject({ kind: 'line' })
  })
})

describe('reconcile (F2 reconnect backfill)', () => {
  const mk = (serial: string, seq: number, text: string): LogLine =>
    line({ serial, seq, idx: 0, text, time: `2026-01-01T00:00:0${seq}Z` })

  it('a reconnect backfill leaves NO gap and NO duplicate', () => {
    const buffer = [mk('A', 1, 'l1'), mk('A', 2, 'l2'), mk('A', 3, 'l3')]
    const backfill = [mk('A', 2, 'l2'), mk('A', 3, 'l3'), mk('A', 4, 'l4'), mk('A', 5, 'l5')]
    const out = reconcile(buffer, backfill, 100)
    expect(out.map((l) => l.text)).toEqual(['l1', 'l2', 'l3', 'l4', 'l5'])
  })

  it('caps to the newest max lines', () => {
    const buffer = [mk('A', 1, 'l1'), mk('A', 2, 'l2'), mk('A', 3, 'l3')]
    expect(reconcile(buffer, [], 2).map((l) => l.text)).toEqual(['l2', 'l3'])
  })

  it('orders interleaved multi-device lines by (time,serial,seq,idx)', () => {
    const a2 = line({ serial: 'A', seq: 2, idx: 0, text: 'a2', time: '2026-01-01T00:00:02Z' })
    const b2 = line({ serial: 'B', seq: 2, idx: 0, text: 'b2', time: '2026-01-01T00:00:02Z' })
    const a1 = line({ serial: 'A', seq: 1, idx: 0, text: 'a1', time: '2026-01-01T00:00:01Z' })
    const out = reconcile([], [b2, a2, a1], 100)
    expect(out.map((l) => l.text)).toEqual(['a1', 'a2', 'b2']) // same time → serial A before B
  })

  it('keyOf distinguishes tokens of a row and falls back to time for null seq', () => {
    expect(keyOf(line({ serial: 'A', seq: 5, idx: 0 }))).not.toBe(keyOf(line({ serial: 'A', seq: 5, idx: 1 })))
    const snap = line({ serial: 'A', seq: null, idx: 0, time: '2026-01-01T00:00:09Z' })
    expect(keyOf(snap)).toContain('t2026-01-01T00:00:09Z')
  })
})

describe('device text stays inert data (F3 — {@html} ban, D19.10)', () => {
  it('a malicious log line round-trips unchanged into a line item, never into a divider', () => {
    const evil = '<script>alert(1)</script> | \n\nevent: log'
    const ev: LogEvent = {
      serial: 'A',
      time: '2026-01-01T00:00:00Z',
      seq: 1,
      boot_count: 1,
      source: 'telemetry',
      gap: true,
      suspect: true,
      lines: [evil],
    }
    const lines = logEventToLines(ev)
    expect(lines[0].text).toBe(evil) // carried verbatim as data, never interpreted
    expect(lines[0].gap).toBe(true) // first token carries the row flags

    const v = synthesizeView(lines)
    const lineItem = v.find((i) => i.kind === 'line')
    expect(lineItem?.kind).toBe('line')
    if (lineItem?.kind === 'line') expect(lineItem.line.text).toBe(evil)
    // synthesized dividers are structured — they never carry device text
    for (const item of v) {
      if (item.kind === 'divider') expect(JSON.stringify(item)).not.toContain('script')
    }
  })

  it('the viewer source binds log text as a text node — no {@html} directive', () => {
    // sanity: ?raw gave the RAW template (a {#each} block compiles away), so the check has teeth
    expect(logViewerSrc).toContain('{#each view as item')
    // match the DIRECTIVE form `{@html <expr>}` (whitespace/paren after @html), not a doc-comment `{@html}`
    expect(logViewerSrc).not.toMatch(/\{@html[\s(]/)
  })
})
