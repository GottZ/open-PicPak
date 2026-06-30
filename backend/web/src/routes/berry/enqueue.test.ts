// Berry enqueue logic — T5 ('*' blast-radius confirm), T7 (read-only degradation), T8 (severing +
// fleet escalation), plus path/body + length cap. Pure node tests; each names the red it guards.

import { describe, expect, it } from 'vitest'
import type { Finding } from '../../lib/berry/lint'
import type { Device } from '../../lib/api/types'
import {
  type Target,
  SCRIPT_MAX,
  buildEnqueue,
  canEnqueue,
  blastRadius,
  riskLevel,
  scriptOk,
} from '../../lib/berry/enqueue'

const ONE: Target = { kind: 'one', serial: 'DEV1' }
const FLEET: Target = { kind: 'fleet' }
const SCRIPT = 'reboot()'
const severingFinding: Finding = { severity: 'warning', kind: 'severing', line: 1, message: 'set_url() is severing' }

function dev(serial: string): Device {
  return { serial, label: null, channel: 'stable', last_seen: null, bonded: false }
}

describe('T5 — "*" blast-radius confirm', () => {
  it('a fleet enqueue is BLOCKED until the operator types exactly "*"', () => {
    expect(canEnqueue(FLEET, '', true, SCRIPT)).toBe(false) // not armed
    expect(canEnqueue(FLEET, 'x', true, SCRIPT)).toBe(false) // wrong token
    expect(canEnqueue(FLEET, '*', true, SCRIPT)).toBe(true) // armed
    // red: a plain submit would broadcast RCE to the whole fleet with one mis-click.
  })

  it('a single-device enqueue needs a selection but no arm token', () => {
    expect(canEnqueue(ONE, '', true, SCRIPT)).toBe(true)
    expect(canEnqueue({ kind: 'one', serial: '' }, '', true, SCRIPT)).toBe(false)
    expect(canEnqueue(null, '', true, SCRIPT)).toBe(false)
  })

  it('blastRadius is the snapshot roster size (D23.6)', () => {
    expect(blastRadius([dev('A'), dev('B'), dev('C')])).toBe(3)
    expect(blastRadius([])).toBe(0)
  })

  it('builds the right path/body for each target (Doc 17 §4.5)', () => {
    expect(buildEnqueue(ONE, SCRIPT, '')).toEqual({
      path: '/api/devices/DEV1/command',
      body: JSON.stringify({ script: SCRIPT, note: undefined }),
    })
    expect(buildEnqueue(FLEET, SCRIPT, 'point at new ingest')).toEqual({
      path: '/api/command',
      body: JSON.stringify({ serial: '*', script: SCRIPT, note: 'point at new ingest' }),
    })
  })
})

describe('T7 — read-only degradation', () => {
  it('a non-admin can never enqueue (single OR fleet), even when otherwise valid', () => {
    expect(canEnqueue(ONE, '', false, SCRIPT)).toBe(false)
    expect(canEnqueue(FLEET, '*', false, SCRIPT)).toBe(false)
    // pairs with the server's requireAdmin 403 (Doc 17 T2) — defense in depth.
  })
})

describe('T8 — severing warning + fleet escalation (D23.8)', () => {
  it('a severing call on a single device is "severing"; under "*" it escalates to "fleet-severing"', () => {
    expect(riskLevel([severingFinding], ONE)).toBe('severing')
    expect(riskLevel([severingFinding], FLEET)).toBe('fleet-severing')
    // red: no risk class → a fleet-wide set_wifi ships unflagged and can strand the whole fleet.
  })

  it('no severing finding → normal, regardless of target', () => {
    expect(riskLevel([], FLEET)).toBe('normal')
    expect(riskLevel([{ severity: 'warning', kind: 'unknown', line: 1, message: 'x' }], FLEET)).toBe('normal')
  })
})

describe('script length cap (mirrors commandstore.ScriptMax)', () => {
  it('blocks empty + oversized, allows in-range', () => {
    expect(scriptOk('')).toBe(false)
    expect(scriptOk('   \n  ')).toBe(false)
    expect(scriptOk('reboot()')).toBe(true)
    expect(scriptOk('a'.repeat(SCRIPT_MAX))).toBe(true)
    expect(scriptOk('a'.repeat(SCRIPT_MAX + 1))).toBe(false)
    // an oversized script also fails canEnqueue (the server 422s past the cap anyway).
    expect(canEnqueue(ONE, '', true, 'a'.repeat(SCRIPT_MAX + 1))).toBe(false)
  })
})
