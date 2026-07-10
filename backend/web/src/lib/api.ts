// Typed fetch wrapper (design 28 §4.3): the admin API authenticates via the
// httpOnly ppk_sid cookie (credentials: 'same-origin') after W8 — the Bearer key
// no longer lives in the JS heap (the XSS-exfil win). Every MUTATING request
// carries X-Requested-With: picpak, the CSRF second layer the server enforces
// with 403 csrf_required beside SameSite=Strict (design §4.1). Normalizes every
// failure shape into ApiError and carries the X-Request-ID so a browser error
// stays greppable in the cmd/admin logs (D17.5). The 401 interceptor routes a
// non-probe rejection back to the login screen via the configured hook.

/** Normalized API failure. `status` 0 means the request never got a response. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string | null
  /**
   * The full `{success:false, …}` envelope on a server error — carries fields
   * beyond `error`, e.g. the server's machine `code` and a 422 field list, for
   * the feature docs' forms. null for network/parse failures.
   */
  readonly details: Record<string, unknown> | null

  constructor(
    status: number,
    code: string,
    message: string,
    requestId: string | null = null,
    details: Record<string, unknown> | null = null,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
  }
}

/** Wrap any thrown value; ApiError instances pass through unchanged. */
export function toApiError(err: unknown): ApiError {
  if (err instanceof ApiError) return err
  return new ApiError(0, 'internal', err instanceof Error ? err.message : String(err))
}

interface ApiHooks {
  /** Fired when a non-probe request is rejected (401) — cookie session expired/revoked → teardown. */
  onUnauthorized: () => void
}

const hooks: ApiHooks = {
  onUnauthorized: () => {},
}

/** Wire the session into the client (called once by auth.svelte.ts). */
export function configureApi(next: Partial<ApiHooks>): void {
  Object.assign(hooks, next)
}

/**
 * Fire the unauthorized teardown hook. The non-JSON transports (api-binary.ts,
 * design 29 §4.2) call this on a 401 so a rejected cookie session tears down the
 * same way as on the apiFetch path (§5 S6). Uploads/frame reads are never boot
 * probes, so this is always the non-probe teardown — no probe branch here.
 */
export function fireUnauthorized(): void {
  hooks.onUnauthorized()
}

export interface ApiFetchOptions {
  /**
   * Marks a boot/login probe (session.restore, the login-time whoami). A 401 on a
   * probe does NOT fire the unauthorized hook — a signed-out visitor is a normal
   * 401, not a mid-session revoke — the caller owns that outcome.
   */
  probe?: boolean
}

// The CSRF header every mutating request carries (design §4.1) — the server enforces it with 403
// csrf_required on cookie-authed mutations; a cross-site <form> POST cannot set a custom header.
// Exported so the non-JSON transports (api-binary.ts) stamp the identical header (design 29 §4.2).
export const CSRF_HEADER = 'X-Requested-With'
export const CSRF_VALUE = 'picpak'

/** Read-only methods take no CSRF header (mirror of adminhttp.csrfSafeMethod). */
export function isSafeMethod(method: string): boolean {
  const m = method.toUpperCase()
  return m === 'GET' || m === 'HEAD' || m === 'OPTIONS'
}

/**
 * Fetch a JSON API endpoint. Throws ApiError for network failures, non-2xx
 * statuses and `{success:false}` envelopes. The admin API uses real status
 * codes (design 19 §2 — no ctxd HTTP-200-with-success:false heartbeat quirk),
 * so the success:false-inside-2xx branch is defensive only.
 */
export async function apiFetch<T>(
  path: string,
  init: RequestInit = {},
  opts: ApiFetchOptions = {},
): Promise<T> {
  const headers = new Headers(init.headers)
  const method = init.method ?? 'GET'
  if (!isSafeMethod(method)) headers.set(CSRF_HEADER, CSRF_VALUE)
  if (init.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }

  let res: Response
  try {
    // credentials: 'same-origin' rides the httpOnly ppk_sid cookie along — the cookie IS the carrier.
    res = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  } catch (cause) {
    const detail = cause instanceof Error ? cause.message : String(cause)
    throw new ApiError(0, 'network', `admin API unreachable: ${detail}`)
  }

  const requestId = res.headers.get('X-Request-ID')
  const body = await parseBody(res)
  const serverError = envelopeError(body)
  const details = asRecord(body)

  if (res.status === 401) {
    if (!opts.probe) hooks.onUnauthorized()
    throw new ApiError(401, 'unauthorized', serverError ?? 'session expired or not signed in', requestId, details)
  }
  if (!res.ok) {
    if (res.status === 403 && details?.['code'] === 'csrf_required') {
      // Regression signal: isSafeMethod adds X-Requested-With to every mutation above, so a
      // csrf_required should be unreachable. Logged (not swallowed) so a future missing-header path is
      // caught rather than silently 403ing a legitimate client (design §4.1).
      console.error(`api.ts: csrf_required on ${method} ${path} — X-Requested-With was not sent`)
    }
    const message = serverError ?? `request failed (HTTP ${res.status})`
    throw new ApiError(res.status, codeFor(res.status), message, requestId, details)
  }
  if (serverError !== null) {
    // success:false inside a 2xx body — surfaced as an error, never as data.
    throw new ApiError(res.status, 'api_error', serverError, requestId, details)
  }
  if (body === undefined) {
    throw new ApiError(res.status, 'invalid_response', 'response was not valid JSON', requestId)
  }
  return body as T
}

/**
 * Parse a body text as JSON, tolerating leading whitespace (RFC 8259 allows it;
 * JSON.parse skips it). Returns undefined for empty or non-JSON text. Exported
 * so the non-JSON transports (api-binary.ts) parse an error envelope — the XHR
 * upload path holds only a `responseText` string, not a Response — with the
 * exact same JSON-parse policy as apiFetch (one contract, no second parser).
 */
export function parseJson(text: string): unknown {
  if (text.trim() === '') return undefined
  try {
    return JSON.parse(text) as unknown
  } catch {
    return undefined
  }
}

/** Parse a Response body as JSON, else undefined (empty/non-JSON). */
async function parseBody(res: Response): Promise<unknown> {
  let text: string
  try {
    text = await res.text()
  } catch {
    return undefined
  }
  return parseJson(text)
}

/** A parsed JSON object body, else null (kept on ApiError.details). Shared with api-binary.ts. */
export function asRecord(body: unknown): Record<string, unknown> | null {
  return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : null
}

/** Extract the error of a `{success:false, error}` envelope, else null. Shared with api-binary.ts. */
export function envelopeError(body: unknown): string | null {
  if (typeof body !== 'object' || body === null) return null
  const envelope = body as { success?: unknown; error?: unknown }
  if (envelope.success !== false) return null
  return typeof envelope.error === 'string' && envelope.error !== '' ? envelope.error : 'request failed'
}

/** Stable machine class per HTTP status (ApiError.code). */
export function codeFor(status: number): string {
  switch (status) {
    case 400:
      return 'bad_request'
    case 403:
      return 'forbidden'
    case 404:
      return 'not_found'
    case 409:
      return 'conflict'
    case 422:
      return 'validation'
    case 429:
      return 'rate_limited'
    default:
      return status >= 500 ? 'server' : `http_${status}`
  }
}
