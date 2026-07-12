// Wave C1 (design 03-fw-command-ready §4.3) — the SPA exit nudge, device-free. The orchestration is pure
// over injected deps (pattern: lib/ota/firmware.ts guardedFirmwareUpload), so every load-bearing property is
// pinned here without hardware:
//   (a) console answers the probe → REFRESH goes over the SAME link, the reset pulse is never fired;
//   (b) console silent → the USB-JTAG reset fallback fires, REFRESH is never written into a dead link (N1);
//   (c) no path EVER sends SLEEP — CONSOLE_SLEEP sets skip_fetch and the device sleeps without polling (N2);
//   (d) the probe is bounded by the orchestration's own timer — a dep that ignores its timeoutMs cannot
//       hang the flow (fake timers).
// What stays ON-DEVICE (stage B, §7 wave-1 gates A/B/SOF): that a real trapped console answers the probe,
// that REFRESH actually reaches run_cycle/c2_poll, and that the reset pulse reboots real silicon.

import { describe, it, expect, vi, afterEach } from 'vitest'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import {
  nudgeDeviceExit,
  NUDGE_PROBE_COMMAND,
  NUDGE_EXIT_COMMAND,
  NUDGE_PROBE_TIMEOUT_MS,
  type NudgeLink,
  type ExitNudgeDeps,
} from './exitNudge'

afterEach(() => {
  vi.useRealTimers()
})

interface FakeOpts {
  /** 'reply' answers every command; 'silent' honors timeoutMs by rejecting; 'hang' never settles (test d). */
  probe: 'reply' | 'silent' | 'hang'
  reopenFails?: boolean
  resetFails?: boolean
  refreshFails?: boolean
}

function fakeDeps(opts: FakeOpts) {
  const sent: string[] = []
  let resets = 0
  let closes = 0
  const link: NudgeLink = {
    sendCommand(line, o) {
      sent.push(line)
      if (opts.probe === 'hang') return new Promise<string>(() => {})
      if (opts.probe === 'silent')
        return new Promise<string>((_res, rej) =>
          setTimeout(() => rej(new Error('timed out waiting for device response')), o?.timeoutMs ?? 30000),
        )
      if (opts.refreshFails && line === NUDGE_EXIT_COMMAND)
        return Promise.reject(new Error('timed out waiting for device response'))
      return Promise.resolve(line === NUDGE_PROBE_COMMAND ? `?    unknown: "${line}" (HELP for help)` : 'OK   -> fetch starting')
    },
    close() {
      closes++
      return Promise.resolve()
    },
  }
  const deps: ExitNudgeDeps = {
    reopen: () => (opts.reopenFails ? Promise.reject(new Error('port gone')) : Promise.resolve(link)),
    hardReset: () => {
      resets++
      return opts.resetFails ? Promise.reject(new Error('open failed')) : Promise.resolve()
    },
    probeTimeoutMs: 50, // keep the real-timer tests fast; (d) exercises the default separately
  }
  return { deps, sent, reset: () => resets, closed: () => closes }
}

describe('nudgeDeviceExit — probe answers (SOF outcome: console alive)', () => {
  it("(a) sends REFRESH over the live link and never fires the reset pulse → 'refresh'", async () => {
    const f = fakeDeps({ probe: 'reply' })
    await expect(nudgeDeviceExit(f.deps)).resolves.toBe('refresh')
    expect(f.sent).toEqual([NUDGE_PROBE_COMMAND, NUDGE_EXIT_COMMAND])
    expect(f.reset()).toBe(0) // never reboot a device that answered — the pulse costs a ~22 s re-render
    expect(f.closed()).toBe(1) // the reopened link is released again
  })

  it("REFRESH lost after a live probe → 'failed' (no blind reset over an answering console)", async () => {
    const f = fakeDeps({ probe: 'reply', refreshFails: true })
    await expect(nudgeDeviceExit(f.deps)).resolves.toBe('failed')
    expect(f.reset()).toBe(0)
    expect(f.closed()).toBe(1)
  })
})

describe('nudgeDeviceExit — probe silent (SOF outcome: console gone)', () => {
  it("(b) falls back to the USB-JTAG reset pulse and never writes REFRESH into the dead link → 'hard-reset'", async () => {
    const f = fakeDeps({ probe: 'silent' })
    await expect(nudgeDeviceExit(f.deps)).resolves.toBe('hard-reset')
    expect(f.reset()).toBe(1)
    expect(f.sent).not.toContain(NUDGE_EXIT_COMMAND) // a REFRESH into a torn-down console lands nowhere (§4.3)
    expect(f.closed()).toBe(1) // link released BEFORE the reset path needs the port
  })

  it("reopen itself failing counts as a non-answer → reset fallback, not an instant 'failed'", async () => {
    const f = fakeDeps({ probe: 'reply', reopenFails: true })
    await expect(nudgeDeviceExit(f.deps)).resolves.toBe('hard-reset')
    expect(f.reset()).toBe(1)
    expect(f.sent).toEqual([])
  })

  it("(fallback error) probe silent AND the reset throws → 'failed' (caller shows the error, pollBond still runs)", async () => {
    const f = fakeDeps({ probe: 'silent', resetFails: true })
    await expect(nudgeDeviceExit(f.deps)).resolves.toBe('failed')
  })
})

describe('nudgeDeviceExit — SLEEP is never sent (design 03 §4.3: SLEEP sets skip_fetch → device never polls)', () => {
  it('(c) across BOTH SOF outcomes, no sendCommand call ever carries SLEEP; the exit command is REFRESH', async () => {
    for (const probe of ['reply', 'silent'] as const) {
      const f = fakeDeps({ probe })
      await nudgeDeviceExit(f.deps)
      for (const line of f.sent) expect(line).not.toMatch(/^\s*SLEEP\b/i)
    }
    expect(NUDGE_EXIT_COMMAND).toBe('REFRESH')
  })
})

describe('nudgeDeviceExit — bounded probe (d)', () => {
  it('a dep that ignores timeoutMs cannot hang the flow: the internal timer forces the fallback', async () => {
    vi.useFakeTimers()
    const f = fakeDeps({ probe: 'hang' })
    delete (f.deps as { probeTimeoutMs?: number }).probeTimeoutMs // exercise the DEFAULT bound
    const p = nudgeDeviceExit(f.deps)
    let settled: string | null = null
    void p.then((v) => (settled = v))
    await vi.advanceTimersByTimeAsync(NUDGE_PROBE_TIMEOUT_MS - 1)
    expect(settled).toBeNull() // not settled a tick early — the bound is the constant, not 0
    await vi.advanceTimersByTimeAsync(2)
    await expect(p).resolves.toBe('hard-reset')
    expect(f.reset()).toBe(1)
    expect(f.sent).not.toContain(NUDGE_EXIT_COMMAND)
  })
})

// --- source pin: the stage-B ordering enroll → nudge → pollBond (design 03 §4.3) -------------------------
// Only the SPA can order the pubkey registration BEFORE the device's first poll; the pin fails if anyone
// moves the nudge before the enroll POST or drops the await before pollBond.
describe('Onboard.svelte wiring (source pin)', () => {
  const src = readFileSync(join(process.cwd(), 'src/routes/onboard/Onboard.svelte'), 'utf8')

  it('runs the nudge AFTER a successful enroll and BEFORE pollBond, awaited', () => {
    const enrolled = src.indexOf('enrolled = true')
    expect(enrolled).toBeGreaterThan(-1)
    const nudge = src.indexOf('await nudgeDeviceExit', enrolled)
    expect(nudge).toBeGreaterThan(enrolled) // nudge only fires once the pubkey is on the server
    const poll = src.indexOf('pollBond()', nudge)
    expect(poll).toBeGreaterThan(nudge) // pollBond starts only after the nudge settled
  })

  it('never mentions SLEEP as a console command and adds no {@html} (N3 belongs to the check gate)', () => {
    expect(src).not.toMatch(/sendCommand\(\s*['"`]SLEEP/)
    expect(src.includes('{@html')).toBe(false)
  })
})
