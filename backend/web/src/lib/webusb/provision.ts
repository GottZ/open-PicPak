// The scripted provisioning sequence (Design 26 §4.2 / D26.4, F3/F4). One ordered pass over the firmware
// setup console: validate every field → SETWIFI → SETURL → NVSSET dev_sn (JSON envelope) → C2 URL →
// C2 PERIOD → SETWAKE → NET TRY → C2 BOND (only on a confirmed connect). The NET-TRY-before-BOND ordering is
// forced by the entropy constraint (c2key.c:17-23): the device's identity keypair must be generated with the
// radio up, or esp_fill_random is a boot-seeded PRNG → a low-entropy private key. This is a UI-LAYER
// mitigation; the durable fix is a firmware BOND-requires-RF guard (Design 26 §8 O7, on-device).

import { ConsoleSession, CONSOLE_TIMEOUT_MS } from './console'
import {
  validateProvisionFields,
  hasErrors,
  lineWithinConsoleCap,
  CON_LINE_MAX,
  type ProvisionFields,
} from './validate'

/**
 * Wrap a bare serial into the on-device NVS value the firmware parses (D26.5): `{"serial_number":"<S>"}`.
 * The firmware stores this blob whole (console.c:202-211) and later extracts the inner field
 * (c2_device_serial, cmd.c:180-202). Writing a BARE serial here breaks the parse → the device never polls /
 * bonds (the §2.2 INERT-feature blocker). The compact object has no internal space (passes the line protocol).
 */
export function wrapSerial(serial: string): string {
  return `{"serial_number":"${serial}"}`
}

/**
 * The firmware's own dev_sn parse (cmd.c:191-201), mirrored: find `"serial_number"`, the colon, the opening
 * quote, copy the inner value up to the closing quote, capped at 31 chars (the on-device sn[32] copy cap,
 * cmd.c:199,314). Returns null on a missing key / unparseable blob (the firmware returns false → CMD_INTENT_NONE).
 * `extractInnerSerial(wrapSerial(S)) === S` for any valid S (D26.5 coherence invariant, F4).
 */
export function extractInnerSerial(nvsValue: string): string | null {
  const key = nvsValue.indexOf('"serial_number"')
  if (key < 0) return null
  const colon = nvsValue.indexOf(':', key + '"serial_number"'.length)
  if (colon < 0) return null
  const openQ = nvsValue.indexOf('"', colon + 1)
  if (openQ < 0) return null
  const closeQ = nvsValue.indexOf('"', openQ + 1)
  if (closeQ < 0) return null
  return nvsValue.slice(openQ + 1, closeQ).slice(0, 31)
}

export interface ProvisionStep {
  line: string
  /** the secret to redact (RX+TX) for this command, if any (D26.10). */
  redact?: string
}

/**
 * The ordered OK/ERR config commands (steps before NET TRY / BOND). NVSSET carries the JSON envelope (D26.5).
 * C2 PERIOD / SETWAKE are omitted when blank (Policy=Data — the operator either sets them or the firmware
 * default holds).
 */
export function buildProvisionSteps(f: ProvisionFields): ProvisionStep[] {
  const steps: ProvisionStep[] = [
    { line: `SETWIFI ${f.ssid} ${f.password}`, redact: f.password || undefined },
    { line: `SETURL ${f.frameUrl}` },
    { line: `NVSSET storage dev_sn ${wrapSerial(f.serial)}` },
    { line: `C2 URL ${f.c2Url}` },
  ]
  const period = (f.c2PeriodSeconds ?? '').trim()
  if (period !== '') steps.push({ line: `C2 PERIOD ${period}` })
  const wake = (f.wakeSeconds ?? '').trim()
  if (wake !== '') steps.push({ line: `SETWAKE ${wake}` })
  return steps
}

export type ProvisionResult = { ok: true; pubkeyHex: string } | { ok: false; reason: string }

export interface RunProvisionOptions {
  waitForBanner?: boolean
  bannerTimeoutMs?: number
  commandTimeoutMs?: number
  netTryTimeoutMs?: number
  bondTimeoutMs?: number
}

/**
 * Run the full D26.4 sequence over an already-started ConsoleSession. Validates first (invalid field → abort
 * before any byte is sent, D26.11). NET TRY awaits the NET-specific result; a FAILED Wi-Fi HARD-STOPS before
 * BOND (never generates the keypair with the radio down). On a confirmed connect it sends C2 BOND and returns
 * the pubkey hex for enrollment. All device errors/timeouts surface as a legible `reason`.
 */
export async function runProvision(
  session: ConsoleSession,
  f: ProvisionFields,
  opts: RunProvisionOptions = {},
): Promise<ProvisionResult> {
  const errors = validateProvisionFields(f)
  if (hasErrors(errors)) {
    return { ok: false, reason: 'invalid fields: ' + Object.values(errors).join('; ') }
  }
  const cmdTimeout = opts.commandTimeoutMs ?? CONSOLE_TIMEOUT_MS
  try {
    if (opts.waitForBanner) {
      // Best-effort: on first boot the device renders the ~22s EPD setup screen before the console banner.
      await session
        .waitForLine(
          (l) => /Setup Console|setup mode|INCOMPLETE|Commands:/i.test(l),
          opts.bannerTimeoutMs ?? cmdTimeout,
        )
        .catch(() => {})
    }
    for (const step of buildProvisionSteps(f)) {
      if (!lineWithinConsoleCap(step.line)) {
        return { ok: false, reason: `built command exceeds the ${CON_LINE_MAX}-char console line cap` }
      }
      const reply = await session.sendCommand(step.line, { redact: step.redact ?? null, timeoutMs: cmdTimeout })
      if (reply.startsWith('ERR')) return { ok: false, reason: `device rejected: ${reply}` }
    }
    const net = await session.sendNetTry(`NET TRY ${f.ssid} ${f.password}`, {
      redact: f.password || null,
      timeoutMs: opts.netTryTimeoutMs ?? cmdTimeout,
    })
    if (net === 'failed') {
      return { ok: false, reason: 'Wi-Fi did not come up; BOND skipped to avoid a low-entropy key' }
    }
    const pubkeyHex = await session.sendBond({ timeoutMs: opts.bondTimeoutMs ?? cmdTimeout })
    return { ok: true, pubkeyHex }
  } catch (e) {
    return { ok: false, reason: (e as Error).message }
  }
}
