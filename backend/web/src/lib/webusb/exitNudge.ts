// SPA exit nudge after enroll (design 03-fw-command-ready §4.3, wave C1). After a successful enroll the
// device still sits trapped in its force_open setup console (console.c:62 — the between-command wait is
// disconnect-only) and never reaches run_cycle/c2_poll, so pollBond would run its 180 s window dry. This
// module nudges it out over the reopened serial link, robust against BOTH SOF outcomes (§4.3):
//
//   console ALIVE  → send REFRESH (console.c:231-233 → CONSOLE_REFRESH → main.c else-branch → run_cycle →
//                    field poll). The first poll finds the pubkey the enroll just registered → bond within
//                    one poll. NEVER SLEEP: CONSOLE_SLEEP sets skip_fetch and the device deep-sleeps WITHOUT
//                    ever polling (§4.3) — the N2 test pins this. REFRESH also cleanly consumes a pending
//                    force_setup flag (§5/B4). PROCEED is not a distinct alternative (§8/E-Cmd).
//   console SILENT → provision()'s port.close() ended the USB enumeration (SOF hypothesis false): the
//                    console gave up, the USB-Serial-JTAG driver is torn down (console.c:517-519), a REFRESH
//                    would land nowhere. Fall back to a USB-JTAG reset pulse (flasher.ts usbJtagHardReset):
//                    the provisioned, config-complete device reboots into a normal run (~22 s EPD re-render)
//                    and polls.
//
// Pure orchestration over injected deps (pattern: lib/ota/firmware.ts guardedFirmwareUpload) — node-testable
// with no hardware (exitNudge.test.ts). The on-device truth (a real console answers, REFRESH reaches the
// field poll, the pulse reboots silicon) is the stage-B gate (§7 wave-1 gates A/B/SOF, W3).

import type { LogSink } from './types'

/**
 * Probe/exit reply bound. A trapped console answers a line immediately — console.c:62 polls the RX in 500 ms
 * slices and handle() prints its reply synchronously; the 30 s CONSOLE_TIMEOUT_MS exists for commands landing
 * mid-EPD-refresh during provisioning, a state the post-provision prompt is never in. 5 s is generous
 * headroom over the USB round trip while keeping the fallback decision fast (the reset path costs a ~22 s
 * re-render anyway). Injectable per call for tests.
 */
export const NUDGE_PROBE_TIMEOUT_MS = 5000

/**
 * The liveness probe is a deliberate UNKNOWN command: the console replies `?    unknown: "PING" (HELP for
 * help)` (console.c:459), which ConsoleSession's okErr matcher (`^(OK|ERR|\?)`) resolves — a clean echo with
 * zero side effects. A banner-read is NOT usable here: "=== PicPak Setup Console ===" (console.c:472) prints
 * once at console ENTRY (boot), never on a serial reopen, so waiting for it would false-negative on a
 * healthy trapped console.
 */
export const NUDGE_PROBE_COMMAND = 'PING'

/** The only correct exit trigger (§4.3/§8 E-Cmd): REFRESH → run_cycle → field poll. Never SLEEP. */
export const NUDGE_EXIT_COMMAND = 'REFRESH'

/** Which path carried the nudge — the page renders this as the step-5 status line. */
export type NudgeOutcome = 'refresh' | 'hard-reset' | 'failed'

/** The reopened console surface (ConsoleSession.sendCommand is a structural match) plus its release. */
export interface NudgeLink {
  /** Send a console line and resolve with the first OK/ERR/? reply, bounded by opts.timeoutMs. */
  sendCommand(line: string, opts?: { timeoutMs?: number }): Promise<string>
  /** Release reader/writer + port so the reset fallback (or the operator) can reopen it. */
  close(): Promise<void>
}

/** Injected transports — the page wires webSerialLink/ConsoleSession + usbJtagHardReset; tests wire fakes. */
export interface ExitNudgeDeps {
  /** Reopen the authorized port at console baud (the Onboard.svelte:250-253 stale-handle pattern). */
  reopen(): Promise<NudgeLink>
  /** SOF fallback: pulse the USB-JTAG reset (flasher.ts usbJtagHardReset over a fresh transport). */
  hardReset(): Promise<void>
  /** Probe/exit reply bound in ms; default NUDGE_PROBE_TIMEOUT_MS. */
  probeTimeoutMs?: number
  log?: LogSink
}

/**
 * Race a dep promise against the orchestration's OWN timer, so a dep that mishandles its timeoutMs can never
 * hang the onboarding flow (test d). A late settle of the loser is absorbed by the already-settled Promise.
 */
function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(`no console reply within ${ms} ms`)), ms)
    p.then(
      (v) => {
        clearTimeout(t)
        resolve(v)
      },
      (e: unknown) => {
        clearTimeout(t)
        reject(e instanceof Error ? e : new Error(String(e)))
      },
    )
  })
}

/**
 * The §4.3 sequence: reopen → probe → REFRESH ('refresh') | silence → reset pulse ('hard-reset') | error in
 * the chosen path → 'failed'. Never throws — the caller shows the outcome and starts pollBond REGARDLESS
 * (even 'failed': the device can still bond, e.g. via a later self-exit). A reopen failure counts as a
 * non-answer (the console is unreachable either way) and falls through to the reset pulse; a lost REFRESH
 * reply after a LIVE probe returns 'failed' WITHOUT a reset — the device most likely took the command and is
 * tearing down its USB driver (console.c:516-519), and a blind ~22 s reboot on top would be worse.
 */
export async function nudgeDeviceExit(deps: ExitNudgeDeps): Promise<NudgeOutcome> {
  const log = deps.log ?? (() => {})
  const timeoutMs = deps.probeTimeoutMs ?? NUDGE_PROBE_TIMEOUT_MS
  let link: NudgeLink | null = null
  try {
    link = await deps.reopen()
  } catch (e) {
    log('Console reopen failed (' + (e as Error).message + ') — treating as silent.', 'dim')
  }
  if (link) {
    let alive = false
    try {
      await withTimeout(link.sendCommand(NUDGE_PROBE_COMMAND, { timeoutMs }), timeoutMs)
      alive = true
    } catch {
      log('Setup console did not answer the probe — falling back to a USB-JTAG reset.', 'info')
    }
    if (alive) {
      try {
        await withTimeout(link.sendCommand(NUDGE_EXIT_COMMAND, { timeoutMs }), timeoutMs)
        log('Console alive — REFRESH sent; the device leaves setup and polls.', 'ok')
        return 'refresh'
      } catch (e) {
        log('REFRESH reply lost (' + (e as Error).message + ') — leaving the device be.', 'err')
        return 'failed'
      } finally {
        await link.close().catch(() => {})
      }
    }
    await link.close().catch(() => {}) // release the port BEFORE the reset path reopens it
  }
  try {
    await deps.hardReset()
    log('Reset pulse sent — the device reboots into a normal run and polls.', 'ok')
    return 'hard-reset'
  } catch (e) {
    log('Exit nudge failed: ' + (e as Error).message, 'err')
    return 'failed'
  }
}
