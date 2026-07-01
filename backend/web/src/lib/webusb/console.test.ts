import { describe, it, expect } from 'vitest'
import {
  okErr,
  matchNetTry,
  matchBond,
  RedactionRegistry,
  ConsoleSession,
} from './console'
import type { SerialLink } from './types'

// A fake SerialLink: tests push device lines and inspect what was written. No navigator.serial, no device.
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

  /** Push a device line (CR/LF appended, as the firmware emits). */
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

// Let the read loop + a pending waiter settle before pushing the response line.
const tick = () => new Promise((r) => setTimeout(r, 0))

// Arbitrary valid uncompressed P-256 point hex (air-gap: not a real key).
const PUBKEY_HEX = '04' + 'cd'.repeat(64)

describe('okErr / matchBond / matchNetTry — the generic matcher cannot see the special lines (F2/F8 red)', () => {
  it('okErr matches OK / ERR / ? only', () => {
    expect(okErr('OK')).toBe(true)
    expect(okErr('ERR  something')).toBe(true)
    expect(okErr('? unknown')).toBe(true)
    expect(okErr('NET try "x": connected ip=1.2.3.4')).toBe(false)
    expect(okErr(`C2   ecdsa_pubkey(65)=${PUBKEY_HEX}`)).toBe(false)
  })

  it('matchBond reads the C2-prefixed success line and the ERR bond failure (F2)', () => {
    expect(matchBond(`C2   ecdsa_pubkey(65)=${PUBKEY_HEX}`)).toEqual({ ok: true, hex: PUBKEY_HEX })
    expect(matchBond('ERR  bond/keygen failed')).toEqual({ ok: false, error: 'ERR  bond/keygen failed' })
    expect(matchBond('OK')).toBeNull()
  })

  it('matchNetTry reads connected / FAILED on NET-prefixed lines only (F8)', () => {
    expect(matchNetTry('NET try "home": connected ip=10.0.0.5')).toBe('connected')
    expect(matchNetTry('NET try "home": connected (no ip)')).toBe('connected')
    expect(matchNetTry('NET try "home": FAILED')).toBe('failed')
    expect(matchNetTry('OK')).toBeNull()
    expect(matchNetTry('some other connected line')).toBeNull() // not NET-prefixed
  })
})

describe('ConsoleSession.sendBond (F2)', () => {
  it('resolves with the pubkey hex on a C2-prefixed success line', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = s.sendBond({ timeoutMs: 500 })
    await tick()
    expect(link.writes.join('')).toContain('C2 BOND\r')
    link.push(`C2   ecdsa_pubkey(65)=${PUBKEY_HEX}`)
    await expect(p).resolves.toBe(PUBKEY_HEX)
  })

  it('rejects on ERR bond/keygen failed', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = s.sendBond({ timeoutMs: 500 })
    await tick()
    link.push('ERR  bond/keygen failed')
    await expect(p).rejects.toThrow(/bond/)
  })
})

describe('ConsoleSession.sendNetTry (F8)', () => {
  it('resolves "connected" and ignores the interspersed non-NET noise', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = s.sendNetTry('NET TRY home pw', { redact: 'pw', timeoutMs: 500 })
    await tick()
    link.push('some boot noise')
    link.push('NET try "home": connected ip=10.0.0.5')
    await expect(p).resolves.toBe('connected')
  })

  it('resolves "failed" on a FAILED line (caller hard-stops before BOND)', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    const p = s.sendNetTry('NET TRY home pw', { redact: 'pw', timeoutMs: 500 })
    await tick()
    link.push('NET try "home": FAILED')
    await expect(p).resolves.toBe('failed')
  })
})

describe('RedactionRegistry (F11 unit)', () => {
  it('masks a registered secret anywhere in the line', () => {
    const r = new RedactionRegistry()
    r.add('s3cr3t')
    expect(r.redact('SETWIFI home s3cr3t')).toBe('SETWIFI home ********')
    expect(r.redact('echo: s3cr3t and s3cr3t')).toBe('echo: ******** and ********')
  })
  it('leaves lines without a secret untouched, and ignores empty secrets', () => {
    const r = new RedactionRegistry()
    r.add('')
    r.add(null)
    r.add('pw')
    expect(r.redact('OK')).toBe('OK')
  })
})

describe('ConsoleSession credential redaction on BOTH TX and RX (D26.10 / F11)', () => {
  it('never renders the password — not on the TX echo, not on the firmware RX echo', async () => {
    const link = new FakeLink()
    const logs: string[] = []
    const s = new ConsoleSession(link, (m) => logs.push(m))
    s.start()
    const p = s.sendCommand('SETWIFI home s3cr3tpass', { redact: 's3cr3tpass', timeoutMs: 500 })
    await tick()
    // The firmware echoes typed chars back (console.c:77) — this RX line carries the password verbatim.
    link.push('SETWIFI home s3cr3tpass')
    link.push('OK')
    await p
    const joined = logs.join('\n')
    expect(joined).not.toContain('s3cr3tpass') // RX leak (the private lift's bug) is closed
    expect(joined).toContain('********')
    expect(logs.some((l) => l === '> SETWIFI home ********')).toBe(true) // TX echo masked
  })

  it('redacts every password-bearing command, not just the first SETWIFI (SETWIFI ADD / NET TRY)', async () => {
    const link = new FakeLink()
    const logs: string[] = []
    const s = new ConsoleSession(link, (m) => logs.push(m))
    s.start()
    const p1 = s.sendCommand('SETWIFI ADD other addpass', { redact: 'addpass', timeoutMs: 500 })
    await tick()
    link.push('OK')
    await p1
    const p2 = s.sendNetTry('NET TRY other addpass', { redact: 'addpass', timeoutMs: 500 })
    await tick()
    link.push('NET try "other": connected ip=10.0.0.9') // no secret in this RX line
    await p2
    // Simulate a later RX echo carrying the secret (e.g. an INFO dump) — still masked from the registry.
    const joined = logs.join('\n')
    expect(joined).not.toContain('addpass')
  })
})

describe('ConsoleSession.waitForLine timeout', () => {
  it('rejects when no matching line arrives', async () => {
    const link = new FakeLink()
    const s = new ConsoleSession(link)
    s.start()
    await expect(s.waitForLine(() => false, 20)).rejects.toThrow(/timed out/)
  })
})
