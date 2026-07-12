// Rollout-Panel Pure-Core (design 01-ota-spa §4.4, W4). Extracted node-testbare
// Funktionen für die zwei nicht-trivialen Zweige des Panels: den 404-gone-
// Nebenläufigkeitspfad und die Serial-Client-Validierung — so sind sie über
// vitest echt geprüft statt nur source-gepinnt (Muster lib/ota/firmware.ts).

import { toApiError } from '../api'
import { otaErrorText } from './firmware'
import { m } from '../../paraglide/messages.js'

// Spiegel von ota_http.go:168-175: serial ist entweder der Fleet-Wildcard '*'
// oder ein Serial, das devicestore.ValidSerial akzeptiert. Rein advisorisch
// (der Server bleibt die Wahrheit, 422 invalid_serial) — spiegelt aber exakt,
// damit ein offensichtlich Invalides nie den Wire-Roundtrip macht (§4.4).
const SERIAL_RE = /^[A-Za-z0-9_-]{1,31}$/

/** Client-Validierungs-Spiegel von ota_http.go:168-175. */
export function isValidRolloutSerial(serial: string): boolean {
  return serial === '*' || SERIAL_RE.test(serial)
}

/**
 * §4.4 404-gone-Nebenläufigkeitskante: der `ota`-SSE-Hint (E2/A6) konvergiert
 * die Liste best-effort, nicht transaktional — im Fenster zwischen fremdem
 * Delete und eigenem Reload kann ein PATCH/DELETE eine Zeile treffen,
 * die ein anderer Operator gerade gelöscht hat. `status===404` ist der
 * Normalfall; `details.code==='not_found'` ist belt-and-suspenders für den Fall,
 * dass der Envelope-Code ohne den HTTP-Status durchgereicht wird.
 */
export function isRolloutGone(err: unknown): boolean {
  const e = toApiError(err)
  return e.status === 404 || e.details?.['code'] === 'not_found'
}

/**
 * Lokalisierter Text für eine Rollout-Mutations-Ablehnung. KRITISCH: der
 * 404-gone-Fall wird HIER VOR jedem Delegieren an `otaErrorText` abgefangen —
 * `otaErrorText`s generischer `not_found`-Case zeigt `ota.channel.error.not_found`
 * ("Channel nicht gefunden"), was für einen bereits gelöschten Rollout falsch
 * wäre (§4.4). Alle anderen Codes (inkl. der beiden hier neuen Rollout-Cases
 * `unknown_serial`/`unknown_channel_or_version`, siehe firmware.ts) laufen durch
 * den gemeinsamen Helfer.
 */
export function rolloutErrorText(err: unknown): string {
  if (isRolloutGone(err)) return m['ota.rollout.error.gone']()
  return otaErrorText(err)
}
