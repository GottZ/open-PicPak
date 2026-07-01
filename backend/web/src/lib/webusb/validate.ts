// Operator-field validation before any byte is sent (Design 26 §4.2 / D26.11, F9). The firmware setup
// console is a LINE PROTOCOL: read_line terminates a command on CR or LF (console.c:69) and handle tokenizes
// on spaces — so an unvalidated field carrying \r/\n INJECTS a second console command (a crafted "serial"
// could append an NVSSET), and a space/quote can split or corrupt a value. Every field is validated here,
// before transmit; provision.ts calls this first and aborts on any error.

/** CON_LINE_MAX (console.c:23) — the full built command line must fit; the raw dev_sn NVS read is raw[160]. */
export const CON_LINE_MAX = 160

/** Serial charset valid in ALL consumers at once (D26.11): the JSON envelope (no "/\\/space), the on-device
 *  sn[32] copy cap (≤31, cmd.c:199,314), the ?sn=<S> URL (unescaped, cmd.c:349), and Doc 17 §4.4
 *  devicestore.ValidSerial. A strict subset of any reasonable server rule. Not '*'. */
export const SERIAL_RE = /^[A-Za-z0-9_-]{1,31}$/

// Control chars incl. CR (\x0d), LF (\x0a), NUL (\x00) — the line-split injection vector — and DEL (\x7f).
const CONTROL_RE = /[\u0000-\u001f\u007f]/

const SSID_MAX = 32 // IEEE 802.11 SSID cap
const PASS_MAX = 63 // WPA2 passphrase cap
const URL_MAX = 120 // generous; keeps the built C2 URL/SETURL line inside CON_LINE_MAX

export interface ProvisionFields {
  serial: string
  ssid: string
  password: string
  frameUrl: string // SETURL <url>
  c2Url: string // C2 URL <url>
  c2PeriodSeconds?: string // C2 PERIOD <s> (optional numeric)
  wakeSeconds?: string // SETWAKE <s> (optional numeric)
}

export type FieldErrors = Partial<Record<keyof ProvisionFields, string>>

/** Serial: charset + the on-device 31-char cap. A control char / space / quote / 40-char value is rejected. */
export function validateSerial(serial: string): string | null {
  if (serial === '') return 'serial is required'
  if (!SERIAL_RE.test(serial)) {
    return 'serial must match ^[A-Za-z0-9_-]{1,31}$ (no spaces/quotes; the on-device cap is 31 chars)'
  }
  return null
}

/** SSID: non-empty, no control char, no space (the console tokenizes SETWIFI on spaces — SSID is token 1). */
export function validateSsid(ssid: string): string | null {
  if (ssid === '') return 'Wi-Fi SSID is required'
  if (CONTROL_RE.test(ssid)) return 'SSID must not contain control characters'
  // TODO(spaced-ssid): a length-delimited SETWIFI form would onboard spaced SSIDs (Design 26 G31). For now
  // the console line protocol tokenizes on spaces, so a spaced SSID is not onboardable via this guided flow.
  if (/\s/.test(ssid)) return 'SSID must not contain spaces (console line-protocol limitation)'
  if (ssid.length > SSID_MAX) return `SSID must be ≤ ${SSID_MAX} characters`
  return null
}

/** Password: control-char rejected (CR/LF injection), length-capped. May be empty (open network). Spaces OK
 *  (rest-of-line). */
export function validatePassword(password: string): string | null {
  if (CONTROL_RE.test(password)) return 'Wi-Fi password must not contain control characters'
  if (password.length > PASS_MAX) return `Wi-Fi password must be ≤ ${PASS_MAX} characters`
  return null
}

/** A device URL (frame or C2): https-only (the SETURL/C2 URL firmware gate, console.c:435), control-char
 *  rejected, length-capped. */
export function validateUrl(url: string, label: string): string | null {
  if (url === '') return `${label} is required`
  if (CONTROL_RE.test(url)) return `${label} must not contain control characters`
  if (!/^https:\/\//.test(url)) return `${label} must be an https:// URL (firmware requires TLS)`
  if (url.length > URL_MAX) return `${label} must be ≤ ${URL_MAX} characters`
  return null
}

/** An optional non-negative integer seconds field (C2 PERIOD / SETWAKE). Blank = omit the command. */
export function validateSeconds(value: string | undefined, label: string): string | null {
  if (value === undefined || value.trim() === '') return null
  if (!/^\d+$/.test(value.trim())) return `${label} must be a whole number of seconds`
  return null
}

/** Validate every provision field. Returns a field-keyed error map ({} = all valid). */
export function validateProvisionFields(f: ProvisionFields): FieldErrors {
  const errors: FieldErrors = {}
  const serial = validateSerial(f.serial)
  if (serial) errors.serial = serial
  const ssid = validateSsid(f.ssid)
  if (ssid) errors.ssid = ssid
  const password = validatePassword(f.password)
  if (password) errors.password = password
  const frameUrl = validateUrl(f.frameUrl, 'Frame URL')
  if (frameUrl) errors.frameUrl = frameUrl
  const c2Url = validateUrl(f.c2Url, 'C2 URL')
  if (c2Url) errors.c2Url = c2Url
  const period = validateSeconds(f.c2PeriodSeconds, 'C2 period')
  if (period) errors.c2PeriodSeconds = period
  const wake = validateSeconds(f.wakeSeconds, 'Wake interval')
  if (wake) errors.wakeSeconds = wake
  return errors
}

export function hasErrors(errors: FieldErrors): boolean {
  return Object.keys(errors).length > 0
}

/** A built console line is within the firmware line cap (defense-in-depth; validate keeps fields short). */
export function lineWithinConsoleCap(line: string): boolean {
  return line.length <= CON_LINE_MAX
}
