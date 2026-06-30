// Berry feedback model — T4 (the honesty property, negatively probed): cursor-advance is "applied =
// delivered + attempted", NEVER "succeeded". Plus the '*' N-of-M live merge, monotonic cursor folding,
// and the rehydration→SSE seam. Pure node tests.

import { describe, expect, it } from 'vitest'
import berryEditorSrc from './BerryEditor.svelte?raw'
import {
  type RecentCommand,
  type CursorMap,
  resolveCommand,
  buildFeedback,
  applyCursorEvent,
  parseCursorEvent,
} from '../../lib/berry/feedback'

const SUCCESS_WORDS = /success|succeeded|✓|✔|done\b/i

function cmd(p: Partial<RecentCommand> & { seq: number; serial: string }): RecentCommand {
  return {
    note: null,
    created_at: '2026-06-30T12:00:00Z',
    operator_key_id: null,
    target_count: p.serial === '*' ? 3 : 1,
    applied_count: 0,
    ...p,
  }
}

describe('T4 — cursor-advance is "applied", not "succeeded" (negatively probed)', () => {
  it('a script whose cursor crossed seq shows applied + delivered/attempted, NEVER success', () => {
    // the fixture script "failed to compile" — but the model CANNOT know that (no result channel); the
    // cursor crossed seq, so it shows applied. The honesty is that this is NOT a success claim.
    const c = cmd({ seq: 42, serial: 'DEV1' })
    const cursors: CursorMap = { DEV1: 42 } // cursor reached the enqueued seq
    const row = resolveCommand(c, cursors)

    expect(row.state).toBe('applied')
    expect(row.label).toMatch(/delivered \+ attempted/)
    expect(row.label).not.toMatch(SUCCESS_WORDS) // red: a UI that paints cursor-advance as success lies
    expect(row.logSerial).toBe('DEV1') // the deep-link to the only place the real outcome may surface
  })

  it('NO feedback row — in any state — ever claims success', () => {
    const cmds = [
      cmd({ seq: 1, serial: 'DEV1', applied_count: 1 }), // applied
      cmd({ seq: 2, serial: 'DEV2', applied_count: 0 }), // pending
      cmd({ seq: 3, serial: '*', target_count: 3, applied_count: 2 }), // partial
    ]
    for (const row of buildFeedback(cmds, {})) {
      expect(row.label).not.toMatch(SUCCESS_WORDS)
    }
  })
})

describe("'*' N-of-M live merge", () => {
  it('blends the rehydration baseline with live cursor deltas, monotonic, capped at target', () => {
    const c = cmd({ seq: 10, serial: '*', target_count: 3, applied_count: 1 })

    // baseline only: 1 of 3 applied → partial
    expect(resolveCommand(c, {}).appliedCount).toBe(1)
    expect(resolveCommand(c, {}).state).toBe('partial')

    // two devices' cursors now past seq → 2 of 3 (partial), still honest
    const live: CursorMap = { DEVA: 10, DEVB: 12, DEVC: 5 }
    const row = resolveCommand(c, live)
    expect(row.appliedCount).toBe(2)
    expect(row.state).toBe('partial')
    expect(row.targetCount).toBe(3)
    expect(row.logSerial).toBeNull() // no single device to deep-link for a fleet command

    // all three past seq → applied (3 of 3), capped at target even if more cursors exist
    expect(resolveCommand(c, { DEVA: 10, DEVB: 12, DEVC: 99, DEVD: 50 }).appliedCount).toBe(3)
    expect(resolveCommand(c, { DEVA: 10, DEVB: 12, DEVC: 99, DEVD: 50 }).state).toBe('applied')
  })

  it('a single-serial pending command stays pending until its cursor crosses', () => {
    const c = cmd({ seq: 7, serial: 'DEV9' })
    expect(resolveCommand(c, { DEV9: 5 }).state).toBe('pending') // behind
    expect(resolveCommand(c, { DEV9: 7 }).state).toBe('applied') // reached
  })
})

describe('cursor folding + parsing', () => {
  it('applyCursorEvent only moves forward (cursors never regress)', () => {
    let cur: CursorMap = {}
    cur = applyCursorEvent(cur, { serial: 'A', applied_seq: 5, last_poll_at: null })
    expect(cur.A).toBe(5)
    cur = applyCursorEvent(cur, { serial: 'A', applied_seq: 3, last_poll_at: null }) // stale/lower → ignored
    expect(cur.A).toBe(5)
    cur = applyCursorEvent(cur, { serial: 'A', applied_seq: 9, last_poll_at: null })
    expect(cur.A).toBe(9)
  })

  it('parseCursorEvent validates the SSE frame and rejects junk', () => {
    expect(parseCursorEvent({ serial: 'A', applied_seq: 4, last_poll_at: null })).toEqual({
      serial: 'A',
      applied_seq: 4,
      last_poll_at: null,
    })
    expect(parseCursorEvent({ serial: 'A' })).toBeNull() // missing applied_seq
    expect(parseCursorEvent('nope')).toBeNull()
    expect(parseCursorEvent(null)).toBeNull()
  })
})

describe('D19.10 + D23.4 in the component', () => {
  it('the editor renders feedback as text nodes (raw-html directive banned, D19.10)', () => {
    expect(berryEditorSrc).not.toContain('{@html')
    // The D23.4 honesty (no row ever claims success) is enforced by the model — T4 on resolveCommand /
    // buildFeedback above. The component renders {f.label} verbatim, so it cannot fabricate a per-row
    // claim; a crude source grep here only false-positives on the envelope field + the honesty comment.
  })
})
