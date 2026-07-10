// F2 — Session gates (design 28 §4.3): the cookie-session login flow (POST /api/session → whoami),
// boot restore off an existing cookie, uniform 401 / 429 login failures, logout (DELETE /api/session),
// is_admin degradation and the 401-interceptor teardown. fetch is stubbed — plain node environment,
// no cookie jar needed (the client only sets credentials: 'same-origin'; the "cookie" is the server's
// 200-vs-401 whoami verdict here).

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Session, session } from './auth.svelte'
import { apiFetch } from './api'

const WHOAMI_ADMIN = { success: true, kind: 'session', is_admin: true, scopes: ['image:read', 'image:write'], label: 'example-admin' }

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Stub fetch to answer the queued responses in order. */
function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

beforeEach(() => {
  vi.unstubAllGlobals()
  // Reset the module-singleton bound to the api client's 401 interceptor.
  session.whoami = null
  session.notice = null
})

describe('Session.login', () => {
  it('POSTs credentials to /api/session then loads identity from whoami', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, user_id: 1, username: 'admin' }), jsonResponse(200, WHOAMI_ADMIN))

    const s = new Session()
    await s.login('admin', 'pw')

    expect(s.active).toBe(true)
    expect(s.is_admin).toBe(true)
    expect(s.label).toBe('example-admin')

    // First call: the session POST with the credentials body + the CSRF header (mutating request).
    const [url, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/session')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({ username: 'admin', password: 'pw' })
    expect(new Headers(init.headers).get('X-Requested-With')).toBe('picpak')
    expect(init.credentials).toBe('same-origin')
    // Second call: the whoami probe.
    expect(mock.mock.calls[1]?.[0]).toBe('/api/whoami')
  })

  it('throws a uniform 401 on bad credentials and stays inactive', async () => {
    stubFetch(jsonResponse(401, { success: false, error: 'invalid credentials', code: 'unauthorized' }))
    const s = new Session()
    await expect(s.login('admin', 'wrong')).rejects.toMatchObject({ status: 401, code: 'unauthorized' })
    expect(s.active).toBe(false)
  })

  it('surfaces a 429 rate_limited login attempt', async () => {
    stubFetch(jsonResponse(429, { success: false, error: 'too many attempts', code: 'rate_limited' }))
    const s = new Session()
    await expect(s.login('admin', 'pw')).rejects.toMatchObject({ status: 429, code: 'rate_limited' })
    expect(s.active).toBe(false)
  })

  it('reflects is_admin:false for a read-only account', async () => {
    stubFetch(jsonResponse(200, { success: true }), jsonResponse(200, { ...WHOAMI_ADMIN, is_admin: false }))
    const s = new Session()
    await s.login('reader', 'pw')
    expect(s.active).toBe(true)
    expect(s.is_admin).toBe(false)
  })
})

describe('Session pre-login deriveds', () => {
  it('is inactive and read-only before whoami resolves (read-only floor)', () => {
    const s = new Session()
    expect(s.active).toBe(false)
    expect(s.is_admin).toBe(false)
    expect(s.label).toBeNull()
  })
})

describe('Session.restore', () => {
  it('activates from an existing cookie on boot (whoami 200)', async () => {
    stubFetch(jsonResponse(200, WHOAMI_ADMIN))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(true)
    expect(s.is_admin).toBe(true)
  })

  it('stays signed out with NO notice on a 401 (a fresh, never-signed-in visitor)', async () => {
    stubFetch(jsonResponse(401, { success: false, error: 'not signed in', code: 'unauthorized' }))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
    expect(s.notice).toBeNull()
  })

  it('stays signed out on a transient failure (network)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('fetch failed')))
    const s = new Session()
    await s.restore()
    expect(s.active).toBe(false)
  })
})

describe('Session.logout', () => {
  it('DELETEs the session server-side and clears state', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true }),
      jsonResponse(200, WHOAMI_ADMIN),
      jsonResponse(200, { success: true, logged_out: true }),
    )
    const s = new Session()
    await s.login('admin', 'pw')
    await s.logout()
    expect(s.active).toBe(false)

    const [url, init] = mock.mock.calls[2] as [string, RequestInit]
    expect(url).toBe('/api/session')
    expect(init.method).toBe('DELETE')
  })

  it('clears local state even when the DELETE fails (offline / expired)', async () => {
    stubFetch(jsonResponse(200, { success: true }), jsonResponse(200, WHOAMI_ADMIN))
    const s = new Session()
    await s.login('admin', 'pw')
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('offline')))
    await s.logout()
    expect(s.active).toBe(false)
  })
})

describe('401 interceptor wiring (singleton)', () => {
  it('tears the session down when the cookie is revoked mid-session', async () => {
    stubFetch(
      jsonResponse(200, { success: true }),
      jsonResponse(200, WHOAMI_ADMIN),
      jsonResponse(401, { success: false, error: 'session revoked', code: 'unauthorized' }),
    )

    await session.login('admin', 'pw')
    expect(session.active).toBe(true)

    // Any later non-probe call that 401s → interceptor → login screen.
    await expect(apiFetch('/api/command', { method: 'POST', body: '{}' })).rejects.toMatchObject({ status: 401 })
    expect(session.active).toBe(false)
    expect(session.notice).not.toBeNull()
  })
})
