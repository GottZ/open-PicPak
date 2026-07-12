// API-Token-Panel API-Schicht (design 02-settings-spa §7-W4). Muster lib/settings/secrets.ts: dünn,
// pure, kein rohes fetch — jede Mutation läuft über apiFetch (api.ts:95, CSRF-Header + 401-Teardown
// inklusive). Kapselt die drei Routen POST/GET/DELETE /api/tokens (token_http.go:24-28), alle
// RequireAdmin (token_http.go:26-28).
//
// ONCE-SHOWN-INVARIANTE (§7-W4, Muster FunctionsEditor.svelte:292-296 `mintedToken`): mint()/
// mintToken() liefert den Klartext-Token GENAU EINMAL zurück (token_http.go:83-84 `"token": plaintext
// // shown ONCE`). Dieses Modul selbst persistiert ihn NIRGENDS (kein Log, kein Cache) — der Aufrufer
// (TokensPanel.svelte) hält ihn NUR in einem lokalen once-shown $state, nie in einem Draft oder
// localStorage. list()/listTokens() projiziert strukturell NIE ein Secret-Feld: apitoken.List
// (store.go:169-171) nimmt secret_hash nie in die Projektion auf, und TokenMeta unten trägt deshalb
// bewusst kein value/secret/hash-Feld — es kann nicht existieren, weil der Server es nie sendet.

import { apiFetch, toApiError } from '../api'
import { m } from '../../paraglide/messages.js'

const TOKENS_ROUTE = '/api/tokens'

// Policy-Quelle: apitoken.KnownScopes (backend/internal/apitoken/store.go:51-52) — die einzigen Scopes,
// die der Schema-CHECK (api_tokens_scopes_known) akzeptiert; mint() lehnt jeden anderen mit 422
// unknown_scope ab (token_http.go:64-66). Keine Route liefert diese Liste heute (Inventur bestätigt:
// weder token_http.go noch ein /api/config-Zweig projiziert KnownScopes) — das lokale Array spiegelt
// die serverseitige Policy bewusst als Mechanismus=Code hier, Policy=Daten dort; ein Drift zwischen
// beiden würde beim Mint sofort als 422 unknown_scope sichtbar, nie still.
export const KNOWN_SCOPES = ['image:read', 'image:write'] as const
export type TokenScope = (typeof KNOWN_SCOPES)[number]

// Source: token_http.go list() → adminhttp.WriteOK(w, r, map[string]any{"id","token_id","label",
// "scopes","created_at","expires_at","last_used","status"}) (token_http.go:104-116). status wird
// serverseitig aus disabled_at/expires_at abgeleitet (tokenStatus, token_http.go:167-177) — NIE
// secret_hash: apitoken.List (store.go:172-187) scannt nur tokenCols (store.go:75), das Feld existiert
// im Go-Typ Token (store.go:63-73) strukturell gar nicht.
export interface TokenMeta {
  id: number
  token_id: string
  label: string
  scopes: string[]
  created_at: string
  expires_at: string | null
  last_used: string | null
  status: 'active' | 'revoked' | 'expired'
}

export interface TokensResponse {
  success: true
  tokens: TokenMeta[]
}

// Source: token_http.go mint() → adminhttp.WriteOK(w, r, map[string]any{"token","id","token_id",
// "label","scopes","expires_at","created_at"}) (token_http.go:83-91). `token` ist der EINZIGE Ort im
// gesamten Kontrakt, an dem der Klartext je erscheint — once-shown, nie wieder lesbar (weder über GET
// /api/tokens noch sonstwo, secret_hash verlässt den Store nie, store.go:8).
export interface MintTokenResponse {
  success: true
  token: string
  id: number
  token_id: string
  label: string
  scopes: string[]
  expires_at: string | null
  created_at: string
}

export interface MintTokenInput {
  label: string
  scopes: string[]
  /** RFC3339 oder undefined für "kein Ablauf" (token_http.go:42,52-60). */
  expiresAt?: string
}

// Source: token_http.go revoke() → adminhttp.WriteOK(w, r, map[string]any{"revoked": tok.TokenID})
// (token_http.go:150).
export interface RevokeTokenResponse {
  success: true
  revoked: string
}

/** GET /api/tokens (admin) — Meta-Liste, nie secret_hash (token_http.go:97-117). */
export function listTokens(): Promise<TokensResponse> {
  return apiFetch<TokensResponse>(TOKENS_ROUTE)
}

/**
 * POST /api/tokens (admin) — mint (token_http.go:38-92). `expiresAt` fehlt/undefined → der Body trägt
 * `expires_at: null` ("kein Ablauf", token_http.go:53 `body.ExpiresAt != nil && *body.ExpiresAt != ""`
 * lässt null/leer identisch als "kein Ablauf" durch). Der Rückgabewert trägt den Klartext einmalig
 * (§ oben) — dieser Aufruf selbst speichert ihn nirgends.
 */
export function mintToken(input: MintTokenInput): Promise<MintTokenResponse> {
  return apiFetch<MintTokenResponse>(TOKENS_ROUTE, {
    method: 'POST',
    body: JSON.stringify({
      label: input.label,
      scopes: input.scopes,
      expires_at: input.expiresAt ?? null,
    }),
  })
}

/** DELETE /api/tokens/{id} (admin) — soft-revoke, 404 wenn nicht vorhanden (token_http.go:119-151). */
export function revokeToken(id: number): Promise<RevokeTokenResponse> {
  return apiFetch<RevokeTokenResponse>(`${TOKENS_ROUTE}/${id}`, { method: 'DELETE' })
}

// ---- Mint-Form-Validierung (pure, N3) ----

export interface MintFormState {
  label: string
  scopes: string[]
}

/**
 * Pflicht-Label + mindestens ein Scope (§7-W4 Submit-Guard / Gate-Vorgabe "Submit disabled ohne Label
 * oder ohne ≥1 Scope"). Pure, kein i18n — der Server bleibt mit 422 invalid_label (token_http.go:48-51)
 * autoritativ; dies ist nur der clientseitige Vorab-Gate, Muster isValidSecretName (lib/settings/secrets.ts).
 */
export function mintFormValid(f: MintFormState): boolean {
  return f.label.trim() !== '' && f.scopes.length > 0
}

// ---- datetime-local → RFC3339 (pure, N4) ----

/**
 * Konvertiert den Rohwert eines `<input type="datetime-local">` (HTML-Spec-Format
 * `YYYY-MM-DDTHH:mm[:ss]`, LOKALE Zeitzone ohne Offset) in RFC3339 (`time.RFC3339`, das
 * `token_http.go:54 time.Parse(time.RFC3339, *body.ExpiresAt)` erwartet). Ein leerer/whitespace-String
 * bedeutet "kein Ablauf" und wird zu `undefined` — NIE zu einem leeren oder ungültigen Zeitstempel, der
 * server-seitig als 422 invalid_expiry zurückkäme. `new Date(value)` interpretiert den offset-losen
 * String nach HTML-datetime-local-Semantik als LOKALE Zeit; `toISOString()` serialisiert sie korrekt
 * nach UTC — genau die Absicht eines vom Operator gewählten Wandzeit-Ablaufs.
 */
export function datetimeLocalToRFC3339(value: string): string | undefined {
  const trimmed = value.trim()
  if (trimmed === '') return undefined
  const d = new Date(trimmed)
  if (Number.isNaN(d.getTime())) return undefined
  // toISOString() liefert immer `...ss.mmmZ`; Go's time.Parse(time.RFC3339, …) akzeptiert Millisekunden
  // ohnehin (die stdlib erkennt eine optionale Sekundenbruchteil-Komponente auch wenn das Layout keine
  // trägt) — die Millisekunden werden dennoch gestrippt, damit das Ergebnis ein sauberes, testbares
  // RFC3339 ohne Rausch-Suffix ist.
  return d.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

// ---- Fehlerabbildung (§7-W4 Gate a: 422 unknown_scope als Scope-Feld-Fehler, nicht generischer Toast) ----

export type TokenErrorField = 'label' | 'scopes' | 'expires_at' | null

const CODE_FIELD: Record<string, Exclude<TokenErrorField, null>> = {
  invalid_label: 'label',
  unknown_scope: 'scopes',
  invalid_expiry: 'expires_at',
}

function serverCode(err: unknown): string {
  const e = toApiError(err)
  return typeof e.details?.['code'] === 'string' ? (e.details['code'] as string) : e.code
}

/** Welches Formularfeld ein Server-Fehlercode betrifft — treibt die Feld-Fehler-Platzierung (Gate a). */
export function tokenErrorField(err: unknown): TokenErrorField {
  return CODE_FIELD[serverCode(err)] ?? null
}

/**
 * Lokalisierter Fehlertext, geketet auf den Server-Envelope-`code` — Muster settingsErrorText
 * (lib/settings/secrets.ts). Deckt die drei mint-422-Codes (token_http.go:49,56,65) plus den
 * revoke-404 (token_http.go:125,130 `not_found`). Fallback: die rohe ApiError-Message. Der Token-
 * Klartext selbst taucht in KEINEM Zweig auf — diese Funktion nimmt nur einen Fehler entgegen.
 */
export function tokensErrorText(err: unknown): string {
  switch (serverCode(err)) {
    case 'invalid_label':
      return m['settings.tokens.error.invalid_label']()
    case 'unknown_scope':
      return m['settings.tokens.error.unknown_scope']()
    case 'invalid_expiry':
      return m['settings.tokens.error.invalid_expiry']()
    case 'not_found':
      return m['settings.tokens.not_found']()
    default:
      return toApiError(err).message
  }
}

/** Lokalisierter Status-Text für die Meta-Tabelle (token_http.go:168-177 tokenStatus-Werte). */
export function tokenStatusText(status: TokenMeta['status']): string {
  switch (status) {
    case 'active':
      return m['settings.tokens.status.active']()
    case 'revoked':
      return m['settings.tokens.status.revoked']()
    case 'expired':
      return m['settings.tokens.status.expired']()
  }
}
