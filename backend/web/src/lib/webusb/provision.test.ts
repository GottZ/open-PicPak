import { describe, it, expect } from 'vitest'
import {
  wrapSerial,
  extractInnerSerial,
  buildProvisionSteps,
  runProvision,
} from './provision'
import { ConsoleSession } from './console'
import { DEFAULT_C2_PERIOD_SECONDS } from '../onboard/defaults'
import type { SerialLink } from './types'
import type { ProvisionFields } from './validate'

// Same fake link as console.test.ts (kept local so the tests stay independent).
class FakeLink implements SerialLink {
  private queue: (Uint8Array | null)[] = []
  private resolver: ((v: Uint8Array | null) => void) | null = null
  readonly writes: string[] = []
  private readonly enc = new TextEncoder()
  private readonly dec = new TextDecoder()
  read(): Promise<Uint8Array | null> {
    if (this.queue.length) return Promise.resolve(this.queue.shift() ?? null)
    return new Promise((res) => {
      this.resolver = res
    })
  }
  write(bytes: Uint8Array): Promise<void> {
    this.writes.push(this.dec.decode(bytes))
    return Promise.resolve()
  }
  push(line: string): void {
    const chunk = this.enc.encode(line + '\r\n')
    if (this.resolver) {
      const r = this.resolver
      this.resolver = null
      r(chunk)
    } else {
      this.queue.push(chunk)
    }
  }
}

const tick = () => new Promise((r) => setTimeout(r, 0))
const PUBKEY_HEX = '04' + 'ef'.repeat(64)

const fields: ProvisionFields = {
  serial: 'PP-001',
  ssid: 'homenet',
  password: 'secretpw',
  frameUrl: 'https://frames.example/f.bin',
  c2Url: 'https://c2.example/poll',
  c2PeriodSeconds: '1800',
  wakeSeconds: '',
}

describe('dev_sn JSON envelope (D26.5 / F4 — the BLOCKER)', () => {
  it('wraps to the JSON blob and the firmware parse round-trips to the bare inner', () => {
    expect(wrapSerial('PP-001')).toBe('{"serial_number":"PP-001"}')
    expect(extractInnerSerial(wrapSerial('PP-001'))).toBe('PP-001')
    expect(extractInnerSerial(wrapSerial('a-b_c9'))).toBe('a-b_c9')
  })
  it('returns null on a bare serial / unparseable blob (the firmware CMD_INTENT_NONE path)', () => {
    expect(extractInnerSerial('PP-001')).toBeNull() // bare serial → no key → inert feature
    expect(extractInnerSerial('{"other":"x"}')).toBeNull()
  })
  it('the NVSSET step carries the JSON envelope, not the bare serial (F4 red-b guard)', () => {
    const steps = buildProvisionSteps(fields)
    const devsn = steps.find((s) => s.line.includes('dev_sn'))
    expect(devsn?.line).toBe('NVSSET storage dev_sn {"serial_number":"PP-001"}')
  })
})

// E7 / design 03 §4.2+§8-E-Period: c2_period==0 on a tethered device is a DEAD-END (a pre-enroll poll never
// retries the bond — cmd.c:282 keep-awake branch never entered), so the provision path itself must write a
// non-blank period when the operator leaves the field untouched. An explicit 0 stays an explicit operator
// decision (battery-only devices; the next wake cycle picks the poll up) and is NEVER overridden.
describe('C2 PERIOD non-blank default in the provision path (E7, design 03 Welle 4)', () => {
  it('a blank period field still emits C2 PERIOD with the form default (1800)', () => {
    const lines = buildProvisionSteps({ ...fields, c2PeriodSeconds: '' }).map((s) => s.line)
    expect(lines).toContain(`C2 PERIOD ${DEFAULT_C2_PERIOD_SECONDS}`)
  })
  it('a blank period field mirrors a set wake interval (the §7-W4 preferred default)', () => {
    const lines = buildProvisionSteps({ ...fields, c2PeriodSeconds: '  ', wakeSeconds: '900' }).map((s) => s.line)
    expect(lines).toContain('C2 PERIOD 900')
    expect(lines).toContain('SETWAKE 900')
  })
  it('an explicit 0 is respected and NOT overridden by the default (deliberate battery choice)', () => {
    const lines = buildProvisionSteps({ ...fields, c2PeriodSeconds: '0', wakeSeconds: '900' }).map((s) => s.line)
    expect(lines).toContain('C2 PERIOD 0')
    expect(lines).not.toContain('C2 PERIOD 900')
  })
  it('an explicit operator value wins over both wake and the fixed default', () => {
    const lines = buildProvisionSteps({ ...fields, c2PeriodSeconds: '600', wakeSeconds: '900' }).map((s) => s.line)
    expect(lines).toContain('C2 PERIOD 600')
  })
})

describe('buildProvisionSteps order + optional fields', () => {
  it('emits SETWIFI, SETURL, NVSSET, C2 URL, then C2 PERIOD (wake omitted when blank)', () => {
    const lines = buildProvisionSteps(fields).map((s) => s.line)
    expect(lines[0]).toMatch(/^SETWIFI /)
    expect(lines[1]).toBe('SETURL https://frames.example/f.bin')
    expect(lines[2]).toBe('NVSSET storage dev_sn {"serial_number":"PP-001"}')
    expect(lines[3]).toBe('C2 URL https://c2.example/poll')
    expect(lines[4]).toBe('C2 PERIOD 1800')
    expect(lines.some((l) => l.startsWith('SETWAKE'))).toBe(false)
    expect(buildProvisionSteps(fields).find((s) => s.line.startsWith('SETWIFI'))?.redact).toBe('secretpw')
  })
})

// Drive runProvision, auto-answering OK to each config command; the test controls NET TRY / BOND.
async function drive(link: FakeLink, netTry: string, bond?: string) {
  // Answer the 4-5 config commands (SETWIFI/SETURL/NVSSET/C2 URL/C2 PERIOD) with OK as they are written.
  for (let i = 0; i < 5; i++) {
    await tick()
    link.push('OK')
  }
  await tick()
  link.push(netTry)
  if (bond !== undefined) {
    await tick()
    link.push(bond)
  }
}

describe('runProvision RF-up-before-BOND (D26.4 / F3, negatively probed)', () => {
  it('emits NET TRY before C2 BOND and returns the pubkey on a confirmed connect', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = runProvision(s, fields, { commandTimeoutMs: 500 })
    await drive(link, 'NET try "homenet": connected ip=10.0.0.5', `C2   ecdsa_pubkey(65)=${PUBKEY_HEX}`)
    const res = await p
    expect(res).toEqual({ ok: true, pubkeyHex: PUBKEY_HEX })
    const joined = link.writes.join('')
    const netIdx = joined.indexOf('NET TRY')
    const bondIdx = joined.indexOf('C2 BOND')
    expect(netIdx).toBeGreaterThanOrEqual(0)
    expect(bondIdx).toBeGreaterThan(netIdx) // BOND strictly after NET TRY
  })

  it('HARD-STOPS on a FAILED Wi-Fi — C2 BOND is NEVER written (no keygen with the radio down)', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = runProvision(s, fields, { commandTimeoutMs: 500 })
    await drive(link, 'NET try "homenet": FAILED') // no bond line pushed
    const res = await p
    expect(res.ok).toBe(false)
    if (!res.ok) expect(res.reason).toMatch(/Wi-Fi did not come up/)
    expect(link.writes.join('')).not.toContain('C2 BOND')
  })

  it('never sends a byte for invalid fields (validate-before-transmit, D26.11)', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const res = await runProvision(s, { ...fields, serial: 'bad serial!' }, { commandTimeoutMs: 200 })
    expect(res.ok).toBe(false)
    expect(link.writes.length).toBe(0)
  })

  it('aborts (no BOND) when a config command is rejected with ERR', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = runProvision(s, fields, { commandTimeoutMs: 500 })
    await tick()
    link.push('ERR  setwifi bad args') // first command rejected
    const res = await p
    expect(res.ok).toBe(false)
    expect(link.writes.join('')).not.toContain('C2 BOND')
    expect(link.writes.join('')).not.toContain('NET TRY')
  })
})
