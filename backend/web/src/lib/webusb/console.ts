// The firmware setup-console line protocol (Design 26 §4.3), lifted from the private web/onboard/app.js:301-344
// with three deliberate changes; the timings/lifecycle are otherwise byte-for-byte:
//  (1) BOND + NET-TRY matchers — line shapes the generic /^(OK|ERR|\?)/ can never match (console.c:368-369
//      NET-prefixed; console.c:447 C2-prefixed), so reusing okErr times out → a false failure (F2/F8).
//  (2) RX-side credential redaction — the firmware echoes typed characters (console.c:77 putchar), so the
//      password rides back on the RX capture the private reader logged verbatim (app.js:315). We mask every
//      registered secret before logging, on BOTH the TX echo and the RX capture, for EVERY password-bearing
//      command (SETWIFI / SETWIFI ADD / NET TRY) — D26.10 / F11.
//  (3) pre-send field validation lives in validate.ts (W2).
// The 30 s per-command timeout stays (the device may be mid EPD refresh, ~22 s, when a command lands —
// app.js:343); BOND additionally tolerates first-call keygen latency inside that budget (§2.3 on-device gate).

import type { SerialLink, LogSink } from './types'

export const CONSOLE_TIMEOUT_MS = 30000

// --- pure line matchers (device-free testable) -----------------------------

/** Generic setup-console reply matcher: normal replies start OK / ERR / ? (console.c). */
export const okErr = (line: string): boolean => /^(OK|ERR|\?)/.test(line)

export type NetTryResult = 'connected' | 'failed'

/**
 * `NET TRY` result matcher (console.c:368-369): `NET try "<ssid>": connected ip=…` / `connected (no ip)` /
 * `NET try "<ssid>": FAILED`. NET-prefixed, so okErr never matches it (F8). null on any other line.
 */
export function matchNetTry(line: string): NetTryResult | null {
  if (!/^NET\b/.test(line)) return null
  if (/\bconnected\b/i.test(line)) return 'connected'
  if (/\bFAILED\b/.test(line)) return 'failed'
  return null
}

export type BondResult = { ok: true; hex: string } | { ok: false; error: string }

/**
 * `C2 BOND` result matcher (console.c:447-451): success `C2   ecdsa_pubkey(65)=<130 hex>`, failure
 * `ERR  bond/keygen failed`. The success line is C2-prefixed (not OK), so okErr times out on it → a false
 * "BOND failed" on a healthy device (F2). null on any other line.
 */
export function matchBond(line: string): BondResult | null {
  const m = /ecdsa_pubkey\((\d+)\)=([0-9a-fA-F]+)/.exec(line)
  if (m) return { ok: true, hex: m[2].toLowerCase() }
  if (/^ERR\s+bond/i.test(line)) return { ok: false, error: line }
  return null
}

// --- RX+TX credential redaction (D26.10 / F11) -----------------------------

/**
 * Registry of secrets to mask in every logged line, RX and TX. The firmware echoes typed characters back
 * (console.c:77), so a password appears on the RX capture the reader logs — the TX-only redaction of the
 * private lift (app.js:339) leaks it. Every password-bearing command feeds its secret here; the reader
 * consults the registry before logging any line.
 */
export class RedactionRegistry {
  private secrets: string[] = []

  add(secret: string | null | undefined): void {
    if (secret && secret.length > 0 && !this.secrets.includes(secret)) this.secrets.push(secret)
  }

  /** Mask every registered secret substring. Longest-first so a secret containing another is masked whole. */
  redact(line: string): string {
    let out = line
    for (const s of [...this.secrets].sort((a, b) => b.length - a.length)) {
      if (out.includes(s)) out = out.split(s).join('********')
    }
    return out
  }

  clear(): void {
    this.secrets = []
  }
}

// --- the session ------------------------------------------------------------

interface LineWaiter {
  test(line: string): boolean
}

/**
 * Drives the firmware setup console over a SerialLink. Device-free testable: a fake link feeds device lines,
 * so the matchers, redaction and timeout are all verifiable without hardware (F2/F8/F11). The Web Serial
 * adapter (flasher.ts `webSerialLink`) provides the real link; on-device behaviour (BOND emits a valid key,
 * bonds e2e) is the W3 gate (G1/G2).
 */
export class ConsoleSession {
  private rxBuf = ''
  private waiters: LineWaiter[] = []
  private readonly decoder = new TextDecoder()
  private readonly encoder = new TextEncoder()
  private readLoop: Promise<void> | null = null
  readonly redaction = new RedactionRegistry()

  constructor(
    private readonly link: SerialLink,
    private readonly log: LogSink = () => {},
  ) {}

  /**
   * Start the background read loop: accumulate bytes, split on CR/LF, redact, log, feed waiters
   * (app.js:304-321). Idempotent — a second call is a no-op.
   */
  start(): void {
    if (this.readLoop) return
    this.readLoop = (async () => {
      try {
        for (;;) {
          const chunk = await this.link.read()
          if (chunk === null) break
          this.rxBuf += this.decoder.decode(chunk, { stream: true })
          let nl: number
          while ((nl = this.rxBuf.search(/[\r\n]/)) >= 0) {
            const line = this.rxBuf.slice(0, nl).replace(/\r/g, '')
            this.rxBuf = this.rxBuf.slice(nl + 1)
            if (line.length) {
              this.log(this.redaction.redact(line), 'dim') // RX redaction (D26.10)
              this.waiters = this.waiters.filter((w) => !w.test(line))
            }
          }
        }
      } catch {
        /* link closed / cancelled */
      }
    })()
  }

  /** Resolve with the first line matching `predicate`, or reject after `timeoutMs` (app.js:323-334). */
  waitForLine(predicate: (line: string) => boolean, timeoutMs = CONSOLE_TIMEOUT_MS): Promise<string> {
    return new Promise<string>((resolve, reject) => {
      const w: LineWaiter = {
        test: (line) => {
          if (predicate(line)) {
            clearTimeout(t)
            resolve(line)
            return true
          }
          return false
        },
      }
      const t = setTimeout(() => {
        this.waiters = this.waiters.filter((x) => x !== w)
        reject(new Error('timed out waiting for device response'))
      }, timeoutMs)
      this.waiters.push(w)
    })
  }

  private async sendLine(text: string): Promise<void> {
    await this.link.write(this.encoder.encode(text + '\r'))
  }

  /**
   * Send a command and await the generic OK/ERR/? reply (app.js:338-344). Pass `redact` (the password) for
   * every password-bearing command — it registers the secret for RX+TX masking (D26.10).
   */
  async sendCommand(line: string, opts: { redact?: string | null; timeoutMs?: number } = {}): Promise<string> {
    if (opts.redact) this.redaction.add(opts.redact)
    this.log('> ' + this.redaction.redact(line), 'info') // TX redaction
    await this.sendLine(line)
    return this.waitForLine(okErr, opts.timeoutMs ?? CONSOLE_TIMEOUT_MS)
  }

  /**
   * Send `NET TRY <ssid> <pass>` and await the NET-specific result — `connected` ⇒ proceed, `failed` ⇒ the
   * caller hard-stops before BOND (D26.4/F8). Never the OK/ERR matcher. The password is redacted RX+TX.
   */
  async sendNetTry(line: string, opts: { redact?: string | null; timeoutMs?: number } = {}): Promise<NetTryResult> {
    if (opts.redact) this.redaction.add(opts.redact)
    this.log('> ' + this.redaction.redact(line), 'info')
    await this.sendLine(line)
    const resultLine = await this.waitForLine((l) => matchNetTry(l) !== null, opts.timeoutMs ?? CONSOLE_TIMEOUT_MS)
    return matchNetTry(resultLine) as NetTryResult
  }

  /**
   * Send `C2 BOND` and await the BOND-specific line — success resolves with the pubkey hex, failure rejects
   * (F2). Never the OK/ERR matcher (the success line is C2-prefixed).
   */
  async sendBond(opts: { timeoutMs?: number } = {}): Promise<string> {
    this.log('> C2 BOND', 'info')
    await this.sendLine('C2 BOND')
    const resultLine = await this.waitForLine((l) => matchBond(l) !== null, opts.timeoutMs ?? CONSOLE_TIMEOUT_MS)
    const r = matchBond(resultLine) as BondResult
    if (!r.ok) throw new Error(r.error)
    return r.hex
  }
}
