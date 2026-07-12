// Secrets-KV API-Schicht (design 02-settings-spa §4, W2). Dünn, pure — kapselt die
// vier Routen (secrets_api.go:25-96) + Fehlerabbildung an EINER Stelle. Muster
// lib/ota/firmware.ts (guardedFirmwareUpload/otaErrorText): kein rohes fetch, jede
// Mutation läuft über apiFetch (api.ts:95 — CSRF-Header + 401-Teardown inklusive).
//
// WRITE-ONLY-INVARIANTE (§5 B1/B2): dieses Modul transportiert `value` GENAU EINMAL
// (der PUT-Request-Body) und liest es NIE aus einer Response — der Server liefert
// es nie zurück (secrets_api.go:56 `{name, action}`, KEIN value). Kein Fingerprint,
// keine Länge, kein Echo. Aufrufer dürfen `value` NIE loggen/draften/toasten (§5).

import { apiFetch, toApiError } from '../api'
import type { SecretsResponse } from '../api/types'
import { m } from '../../paraglide/messages.js'

const SECRETS_ROUTE = '/api/secrets'

function secretRoute(name: string): string {
  return `${SECRETS_ROUTE}/${encodeURIComponent(name)}`
}

// Client-Spiegel von secrets.go:32 `validName` — NUR Vorab-UX (sofortiges Feedback
// vor dem Request). Der Server bleibt autoritativ: eine im Client als "gültig"
// erkannte, aber vom Server abgelehnte Eingabe landet als 422 `invalid_name` und
// läuft durch settingsErrorText — der Client-Guard ist kein Ersatz für die 422-Probe.
const VALID_SECRET_NAME = /^[a-z0-9][a-z0-9._-]{0,127}$/

/** Client-seitiger Vorab-Check gegen den Server-Regex (secrets.go:32). Kosmetisch. */
export function isValidSecretName(name: string): boolean {
  return VALID_SECRET_NAME.test(name)
}

// Source: secrets_api.go put() → adminhttp.WriteOK(w, r, map[string]any{"name":
// name, "action": action}) — action ist "created" bei neuer Row, sonst "rotated".
// Bewusst OHNE value (write-only, §1 Nicht-Ziele).
export interface SecretPutResponse {
  success: true
  name: string
  action: 'created' | 'rotated'
}

// Source: secrets_api.go del() → adminhttp.WriteOK(w, r, map[string]any{"name": name}).
export interface SecretDeleteResponse {
  success: true
  name: string
}

/** GET /api/secrets (admin) — Meta-Liste, nie value/ciphertext/nonce (secrets_api.go:60-67). */
export function listSecrets(): Promise<SecretsResponse> {
  return apiFetch<SecretsResponse>(SECRETS_ROUTE)
}

/**
 * PUT /api/secrets/{name} (admin) — seal + UPSERT (secrets_api.go:25-57). `value`
 * verlässt diese Funktion nur im Request-Body; der Rückgabewert trägt ihn nie.
 */
export function putSecret(name: string, value: string): Promise<SecretPutResponse> {
  return apiFetch<SecretPutResponse>(secretRoute(name), {
    method: 'PUT',
    body: JSON.stringify({ value }),
  })
}

/** DELETE /api/secrets/{name} (admin) — unbedingtes Löschen, 404 wenn nicht vorhanden (secrets_api.go:84-96). */
export function deleteSecret(name: string): Promise<SecretDeleteResponse> {
  return apiFetch<SecretDeleteResponse>(secretRoute(name), { method: 'DELETE' })
}

/**
 * Lokalisierter Fehlertext, geketet auf den Server-Envelope-`code`
 * (ApiError.details['code'], Fallback e.code) — Muster otaErrorText (lib/ota/firmware.ts).
 * Deckt die drei PUT-422/413-Codes (secrets_api.go:28-45) plus den DELETE/GET-404
 * (secrets_api.go:73,92 `not_found`, der eigens für diese Meldung im K2-Key-Satz
 * steht). Fallback für jeden anderen Code: die rohe ApiError-Message — der value
 * selbst taucht in KEINEM Zweig auf (§5 B2).
 */
export function settingsErrorText(err: unknown): string {
  const e = toApiError(err)
  const serverCode = typeof e.details?.['code'] === 'string' ? (e.details['code'] as string) : e.code
  switch (serverCode) {
    case 'invalid_name':
      return m['settings.secrets.error.invalid_name']()
    case 'empty_value':
      return m['settings.secrets.error.empty_value']()
    case 'value_too_large':
      return m['settings.secrets.error.value_too_large']()
    case 'not_found':
      return m['settings.secrets.not_found']()
    default:
      return e.message
  }
}
