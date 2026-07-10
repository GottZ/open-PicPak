// Binary + multipart transport for the admin SPA (design 29 §4.2): the two paths
// apiFetch cannot serve. apiFetch's contract is "JSON in, JSON out" and it stamps
// Content-Type: application/json on any body (api.ts) — a FormData upload would
// lose its multipart boundary. This module is the one home for every non-JSON
// transport: a binary GET/POST that returns an ArrayBuffer (frame download, the
// FaaS test-run) and a multipart upload with real progress events.
//
// Both ride the same cookie session as apiFetch (credentials / withCredentials on
// the httpOnly ppk_sid cookie) and carry the X-Requested-With: picpak CSRF header
// on every mutation. Neither opens a second session path: the 401 teardown
// (fireUnauthorized) and the {success:false} envelope parsing reuse the api.ts
// core (§4.2 — no duplicated auth). apiUpload runs on XMLHttpRequest — fetch has
// no upload-progress event — so it is a STRUCTURALLY second transport: the 401
// teardown and the envelope error are hand-wired in onload, not inherited from a
// fetch Response (§5 S6; the W2 negative probe pins that a 401 is not swallowed).

import {
  ApiError,
  CSRF_HEADER,
  CSRF_VALUE,
  asRecord,
  codeFor,
  envelopeError,
  fireUnauthorized,
  isSafeMethod,
  parseJson,
} from './api'

/**
 * Fetch a binary endpoint and return its body as an ArrayBuffer. Mirrors
 * apiFetch's auth + error semantics for a non-JSON RESPONSE: the cookie rides
 * along (credentials: 'same-origin'), a mutation carries the CSRF header, a 401
 * tears the session down (fireUnauthorized), and a non-2xx body is parsed as the
 * {success:false} envelope into an ApiError. On success the raw bytes come back
 * and are never run through JSON parsing. Replaces the hand-rolled fetch block
 * the FaaS test-run used to inline (design 29 §4.2).
 */
export async function apiBinary(path: string, init: RequestInit = {}): Promise<ArrayBuffer> {
  const headers = new Headers(init.headers)
  const method = init.method ?? 'GET'
  if (!isSafeMethod(method)) headers.set(CSRF_HEADER, CSRF_VALUE)

  let res: Response
  try {
    res = await fetch(path, { ...init, headers, credentials: 'same-origin' })
  } catch (cause) {
    const detail = cause instanceof Error ? cause.message : String(cause)
    throw new ApiError(0, 'network', `admin API unreachable: ${detail}`)
  }

  if (res.ok) return res.arrayBuffer()

  // Error path only: a success body is binary and must never be text-parsed; a
  // non-2xx body is the JSON error envelope, read exactly as apiFetch reads it.
  const requestId = res.headers.get('X-Request-ID')
  const body = parseJson(await res.text().catch(() => ''))
  const serverError = envelopeError(body)
  const details = asRecord(body)
  if (res.status === 401) {
    fireUnauthorized()
    throw new ApiError(401, 'unauthorized', serverError ?? 'session expired or not signed in', requestId, details)
  }
  throw new ApiError(res.status, codeFor(res.status), serverError ?? `request failed (HTTP ${res.status})`, requestId, details)
}

/**
 * Upload multipart form data over XMLHttpRequest and parse a JSON envelope
 * response. XHR (not fetch) because only xhr.upload.onprogress yields a real
 * upload-progress fraction; fetch duplex upload streams are not reliable in the
 * target browsers. Content-Type is deliberately NOT set — the browser writes the
 * multipart boundary itself (the whole point; apiFetch's forced application/json
 * would strip it, design 29 §5 S3). The cookie rides via withCredentials and the
 * CSRF header is set explicitly. Because XHR yields no fetch Response, the 401
 * teardown and the envelope error are hand-wired here (§5 S6).
 */
export function apiUpload<T>(path: string, form: FormData, onProgress?: (frac: number) => void): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', path)
    // Ride the httpOnly ppk_sid cookie (mirror of credentials: 'same-origin').
    xhr.withCredentials = true
    // CSRF second layer: multipart/form-data is a "simple request" with no preflight,
    // so a custom header is what a cross-site <form> POST cannot forge (mirror of api.ts).
    xhr.setRequestHeader(CSRF_HEADER, CSRF_VALUE)
    // NB: no setRequestHeader('Content-Type') — the browser sets the multipart boundary.

    if (onProgress) {
      xhr.upload.onprogress = (e: ProgressEvent) => {
        if (e.lengthComputable) onProgress(e.loaded / e.total)
      }
    }

    xhr.onload = () => {
      const requestId = xhr.getResponseHeader('X-Request-ID')
      const body = parseJson(xhr.responseText)
      const serverError = envelopeError(body)
      const details = asRecord(body)
      if (xhr.status === 401) {
        // A rejected cookie session tears down exactly as on the apiFetch path.
        fireUnauthorized()
        reject(new ApiError(401, 'unauthorized', serverError ?? 'session expired or not signed in', requestId, details))
        return
      }
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new ApiError(xhr.status, codeFor(xhr.status), serverError ?? `upload failed (HTTP ${xhr.status})`, requestId, details))
        return
      }
      if (serverError !== null) {
        // success:false inside a 2xx body — surfaced as an error, never as data.
        reject(new ApiError(xhr.status, 'api_error', serverError, requestId, details))
        return
      }
      if (body === undefined) {
        reject(new ApiError(xhr.status, 'invalid_response', 'response was not valid JSON', requestId))
        return
      }
      resolve(body as T)
    }
    xhr.onerror = () => reject(new ApiError(0, 'network', 'admin API unreachable'))
    xhr.ontimeout = () => reject(new ApiError(0, 'network', 'upload timed out'))
    xhr.send(form)
  })
}
