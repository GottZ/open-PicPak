// F1 — api.ts gates (design 19 §6): envelope normalization, error mapping and
// the 401 interceptor. Pure node, fetch stubbed. Test keys assembled at runtime;
// no secret-shaped literals (air-gap repo rule).

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, configureApi, toApiError } from './api'

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ onUnauthorized: () => {} })
})

describe('apiFetch', () => {
  it('returns the parsed body on success', async () => {
    stubFetch(jsonResponse(200, { success: true, label: 'example' }))
    const got = await apiFetch<{ success: boolean; label: string }>('/api/whoami')
    expect(got).toEqual({ success: true, label: 'example' })
  })

  it('rides the cookie with credentials: same-origin and sends no Authorization header', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    await apiFetch('/api/whoami')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(init.credentials).toBe('same-origin')
    expect(new Headers(init.headers).get('Authorization')).toBeNull()
  })

  it('adds the X-Requested-With CSRF header on a mutating request', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    await apiFetch('/api/devices', { method: 'POST', body: '{}' })
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('X-Requested-With')).toBe('picpak')
  })

  it('sends no CSRF header on a safe GET request', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    await apiFetch('/api/whoami')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('X-Requested-With')).toBeNull()
  })

  it('logs a csrf_required 403 as a regression signal (header should always be sent)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'csrf', code: 'csrf_required' }))
    const err = vi.spyOn(console, 'error').mockImplementation(() => {})
    await expect(apiFetch('/api/devices', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
    })
    expect(err).toHaveBeenCalledTimes(1)
    err.mockRestore()
  })

  it('normalizes a success:false envelope inside HTTP 200 (defensive branch)', async () => {
    stubFetch(jsonResponse(200, { success: false, error: 'something failed' }, { 'X-Request-ID': 'req-7' }))
    await expect(apiFetch('/api/devices')).rejects.toMatchObject({
      status: 200,
      code: 'api_error',
      message: 'something failed',
      requestId: 'req-7',
    })
  })

  it('parses bodies with leading whitespace', async () => {
    stubFetch(
      new Response('   \n  {"success":true,"n":1}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    await expect(apiFetch('/api/devices')).resolves.toEqual({ success: true, n: 1 })
  })

  it('fires the unauthorized hook on a 401 (cookie expired/revoked)', async () => {
    stubFetch(jsonResponse(401, { success: false, error: 'session expired', code: 'unauthorized' }, { 'X-Request-ID': 'req-9' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
      requestId: 'req-9',
    })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('does NOT fire the unauthorized hook on a probe request (login/restore)', async () => {
    stubFetch(jsonResponse(401, { success: false, error: 'not signed in', code: 'unauthorized' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami', {}, { probe: true })).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
    })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 403 to forbidden without touching the session', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin privilege required', code: 'forbidden' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/devices', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
      message: 'admin privilege required',
    })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 422 to validation and keeps the envelope on details', async () => {
    stubFetch(jsonResponse(422, { success: false, error: 'bad serial', code: 'invalid_serial' }))
    await expect(apiFetch('/api/devices', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 422,
      code: 'validation',
      details: { code: 'invalid_serial' },
    })
  })

  it('maps 5xx to server with the envelope message', async () => {
    stubFetch(jsonResponse(500, { success: false, error: 'internal error' }))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 500,
      code: 'server',
      message: 'internal error',
    })
  })

  it('maps a fetch rejection to a network ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('fetch failed')))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 0,
      code: 'network',
    })
  })

  it('rejects 2xx non-JSON bodies (proxy error pages)', async () => {
    stubFetch(new Response('<html>gateway</html>', { status: 200 }))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({ code: 'invalid_response' })
  })
})

describe('toApiError', () => {
  it('passes ApiError through and wraps everything else', () => {
    const original = new ApiError(404, 'not_found', 'nope')
    expect(toApiError(original)).toBe(original)
    const wrapped = toApiError(new Error('boom'))
    expect(wrapped).toBeInstanceOf(ApiError)
    expect(wrapped.message).toBe('boom')
    expect(wrapped.status).toBe(0)
  })
})
