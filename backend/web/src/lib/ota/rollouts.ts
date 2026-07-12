// Rollout-Panel Pure-Core (design 01-ota-spa §4.4, W4). Extracted node-testbare
// Funktionen für die zwei nicht-trivialen Zweige des Panels: den 404-gone-
// Nebenläufigkeitspfad und die Serial-Client-Validierung — so sind sie über
// vitest echt geprüft statt nur source-gepinnt (Muster lib/ota/firmware.ts).

import { toApiError } from '../api'
import { otaErrorText } from './firmware'
import type { Source } from './types'
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

// §3/§4.6 Bindestrich-Falle: the resolver's wire value for the channel-default
// source is `'channel-default'` WITH A HYPHEN (internal/rollout/types.go:15,
// lib/ota/types.ts Source), but the i18n key is `ota.resolve.source.channel_default`
// WITH AN UNDERSCORE. A naive `m['ota.resolve.source.' + source]()` targets the
// non-existent key `…source.channel-default` for exactly this one source —
// Paraglide falls back to the raw key string at runtime, not a compile error
// (pinned in ota.test.ts N1). This Record is the explicit, exhaustive mapping;
// TypeScript's `Record<Source, …>` makes a missing source a compile error.
const SOURCE_LABEL: Record<Source, () => string> = {
  serial: () => m['ota.resolve.source.serial'](),
  fleet: () => m['ota.resolve.source.fleet'](),
  'channel-default': () => m['ota.resolve.source.channel_default'](),
  none: () => m['ota.resolve.source.none'](),
}

/**
 * Lokalisiertes Label für einen Resolve-`source`-Tag (§3/§4.6). `source='none'`
 * (unbekanntes Serial ODER kein Ziel, fail-open read.go:18-24, §7-W5) mappt
 * genauso wie jede andere Source auf ihren Text ("kein Ziel") — es ist KEIN
 * Fehlerzustand, siehe N2 in ota.test.ts.
 */
export function resolveSourceLabel(source: Source): string {
  return SOURCE_LABEL[source]()
}

/**
 * Lokalisierter Text für eine PATCH /api/devices/{serial}-Ablehnung (E4,
 * §4.4-Erweiterung/Resolve-Vorschau, devices.go:88-117). KRITISCH wie
 * rolloutErrorText oben: `not_found` heisst hier "kein solches Gerät"
 * (devices.go:111-114, "no such device"), NICHT "Channel nicht gefunden" —
 * otaErrorText's genereller `not_found`-Case (§4.3, Channels) wäre hier
 * falsch und muss VOR jedem Delegieren abgefangen werden, exakt wie
 * `isRolloutGone` den Rollout-404 vor `otaErrorText` abfängt. `unknown_channel`
 * (FK 23503, devices.go:103-106) ist ein weiterer neuer Code, den otaErrorText
 * nicht kennt (es kennt nur `unknown_version`/`unknown_channel_or_version` aus
 * den Channel-/Rollout-Routen) — auch dafür ein eigener Key statt Fallback auf
 * die rohe ApiError-Message.
 */
export function deviceChannelErrorText(err: unknown): string {
  const e = toApiError(err)
  const serverCode = typeof e.details?.['code'] === 'string' ? (e.details['code'] as string) : e.code
  if (serverCode === 'not_found') return m['ota.resolve.error.device_not_found']()
  if (serverCode === 'unknown_channel') return m['ota.resolve.error.unknown_channel']()
  return otaErrorText(err)
}
