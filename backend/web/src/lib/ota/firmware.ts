// Firmware-Upload-Orchestrierung (design 01-ota-spa §4.2, W2). Pure, node-testbar
// mit injiziertem Transport — Muster lib/media/uploader.ts `guardedUpload`. Jeder
// Guard läuft VOR dem Request; ein Guard-Bruch wirft FwGuardError und ruft
// deps.upload NIE (die W2-Negativ-Probe pint genau das, wie N1 in uploader.test.ts).

import { toApiError } from '../api'
import type { FirmwareRegistered } from './types'
import { m } from '../../paraglide/messages.js'

// Spiegel von ota_http.go:23 `maxFirmwareBytes` — ein 200-MB-Blob verlässt den
// Browser nie (§5 B2). Client-Guard, kein Ersatz für den Server-MaxBytesReader.
export const MAX_FIRMWARE_BYTES = 16 << 20

export type FwGuardReason = 'empty' | 'too_large' | 'bad_version'

/** Ein Client-Guard-Bruch — geworfen VOR jedem Upload-Request. */
export class FwGuardError extends Error {
  readonly reason: FwGuardReason
  constructor(reason: FwGuardReason) {
    super(reason)
    this.name = 'FwGuardError'
    this.reason = reason
  }
}

/** Injizierter Transport + Hash — lässt die Orchestrierung ohne DOM node-testen. */
export interface FirmwareUploadDeps {
  /** = sha256Hex (flasher.ts:42-44) — REUSE, kein neuer Hash-Helper. */
  hash: (bytes: Uint8Array) => Promise<string>
  /** Der Multipart-Transport (apiUpload) — nur erreicht, wenn jeder Guard passiert. */
  upload: (form: FormData, onProgress?: (frac: number) => void) => Promise<FirmwareRegistered>
  onProgress?: (frac: number) => void
}

/**
 * Der DoS-/Integritäts-sichere Upload-Pfad (§4.2): Guards → Hash → Upload. Ein
 * Guard-Bruch wirft FwGuardError und ruft deps.upload NIE. Die Versions-Länge
 * wird in UTF-8-BYTES gemessen (`TextEncoder().encode(version).length`), NICHT
 * `.length` (UTF-16-Code-Units) — das spiegelt ota_http.go:61 `len(version) > 31`
 * exakt (Server misst Go-`string`-Bytes). Der Server bleibt die Wahrheit
 * (422 invalid_version) — der Client-Guard ist rein advisorisch (§4.2/§5 B2).
 */
export async function guardedFirmwareUpload(
  file: File,
  version: string,
  deps: FirmwareUploadDeps,
): Promise<FirmwareRegistered> {
  if (file.size === 0) throw new FwGuardError('empty')
  if (file.size > MAX_FIRMWARE_BYTES) throw new FwGuardError('too_large') // nie auf die Wire
  if (version === '' || new TextEncoder().encode(version).length > 31) throw new FwGuardError('bad_version')

  const bytes = new Uint8Array(await file.arrayBuffer())
  const sha256 = await deps.hash(bytes) // client-vorab, Integritäts-Sofortfeedback
  const form = new FormData()
  form.append('version', version)
  form.append('sha256', sha256) // Server RE-HASHT und ist authoritativ (write.go:28)
  form.append('blob', file)
  return deps.upload(form, deps.onProgress)
}

/** Lokalisierter Text für einen Client-Guard-Bruch (Muster uploader.ts guardText). */
export function fwGuardText(reason: FwGuardReason): string {
  switch (reason) {
    case 'empty':
      return m['ota.fw.error.empty']()
    case 'too_large':
      return m['ota.fw.error.too_large']()
    case 'bad_version':
      return m['ota.fw.error.invalid_version']()
  }
}

/**
 * Lokalisierter Text für eine Server-Upload-/Mutations-Ablehnung (§4.5), geket
 * auf den Envelope-`code` (ApiError.details['code'], Fallback e.code) statt auf
 * den HTTP-Status — so tragen `duplicate_version` 409 und `sha_mismatch` 422
 * unterscheidbare Meldungen. Ein Helper für ALLE OTA-Panels (§4.3 "erweitere
 * ihn statt einen zweiten Helper zu bauen"): `unknown_version`/`not_found`
 * kommen von `PUT /api/channels/{name}` (ota_http.go:143,151, §4.3), die
 * übrigen Codes vom Firmware-Upload. Fallback für Codes ohne dedizierten
 * OTA-String: die rohe ApiError-Message.
 */
export function otaErrorText(err: unknown): string {
  const e = toApiError(err)
  const serverCode = typeof e.details?.['code'] === 'string' ? (e.details['code'] as string) : e.code
  switch (serverCode) {
    case 'duplicate_version':
      return m['ota.fw.error.duplicate']()
    case 'sha_mismatch':
      return m['ota.fw.error.corrupt']()
    case 'invalid_sha':
      return m['ota.fw.error.invalid_sha']()
    case 'invalid_version':
      return m['ota.fw.error.invalid_version']()
    case 'blob_too_large':
      return m['ota.fw.error.too_large']()
    case 'unknown_version':
      return m['ota.channel.error.unknown_version']()
    case 'not_found':
      return m['ota.channel.error.not_found']()
    default:
      return e.message
  }
}
