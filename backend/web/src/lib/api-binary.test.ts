// W2 gates (design 29 §4.2/§5 S3+S6, §7 W2): the non-JSON transports. Pure node,
// fetch + XMLHttpRequest stubbed. Two negative probes anchor the wave:
//   (a) apiFetch stamps application/json onto a FormData body — the multipart bug
//       (the boundary is lost) that apiUpload exists to avoid; apiUpload sends the
//       same form with NO Content-Type header, so the browser sets the boundary.
//   (b) a 401 on the apiUpload XHR path must fire the unauthorized teardown — XHR
//       yields no fetch Response, so a swallowed 401 is the failure this pins.

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { apiFetch, configureApi } from './api'
import { apiBinary, apiUpload } from './api-binary'

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

// Minimal XMLHttpRequest double: records the request shape and lets a test drive
// the completion (status + responseText + headers) through respond().
class FakeXHR {
  static instances: FakeXHR[] = []
  method = ''
  url = ''
  withCredentials = false
  sent: unknown = null
  readonly headers: Record<string, string> = {}
  readonly upload: { onprogress: ((e: ProgressEvent) => void) | null } = { onprogress: null }
  onload: (() => void) | null = null
  onerror: (() => void) | null = null
  ontimeout: (() => void) | null = null
  status = 0
  responseText = ''
  #responseHeaders: Record<string, string> = {}

  constructor() {
    FakeXHR.instances.push(this)
  }
  open(method: string, url: string): void {
    this.method = method
    this.url = url
  }
  setRequestHeader(key: string, value: string): void {
    this.headers[key] = value
  }
  getResponseHeader(key: string): string | null {
    return this.#responseHeaders[key] ?? null
  }
  send(body: unknown): void {
    this.sent = body
  }
  /** Drive a server completion into the onload handler. */
  respond(status: number, responseText = '', headers: Record<string, string> = {}): void {
    this.status = status
    this.responseText = responseText
    this.#responseHeaders = headers
    this.onload?.()
  }
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ onUnauthorized: () => {} })
  FakeXHR.instances = []
})

describe('the FormData / multipart bug apiUpload avoids', () => {
  it('apiFetch forces application/json onto a FormData body (boundary lost — the bug)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    const form = new FormData()
    form.append('name', 'poster')
    await apiFetch('/api/media/images', { method: 'POST', body: form })
    const init = mock.mock.calls[0]?.[1] as RequestInit
    // Reproduces the defect: apiFetch is structurally unusable for multipart.
    expect(new Headers(init.headers).get('Content-Type')).toBe('application/json')
  })

  it('apiUpload sends the form with NO Content-Type — plus the cookie + CSRF header', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const form = new FormData()
    form.append('name', 'poster')
    const p = apiUpload<{ success: boolean; id: number }>('/api/media/images', form)
    const xhr = FakeXHR.instances.at(-1)
    expect(xhr).toBeDefined()
    xhr?.respond(200, JSON.stringify({ success: true, id: 7 }))
    await expect(p).resolves.toEqual({ success: true, id: 7 })
    expect(xhr?.headers['Content-Type']).toBeUndefined()
    expect(xhr?.headers['X-Requested-With']).toBe('picpak')
    expect(xhr?.withCredentials).toBe(true)
    expect(xhr?.method).toBe('POST')
  })
})

describe('apiUpload 401 teardown (XHR must not swallow it)', () => {
  it('fires the unauthorized hook and rejects with a 401 ApiError', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    const p = apiUpload('/api/media/images', new FormData())
    const xhr = FakeXHR.instances.at(-1)
    xhr?.respond(401, JSON.stringify({ success: false, error: 'session expired', code: 'unauthorized' }), {
      'X-Request-ID': 'req-401',
    })
    await expect(p).rejects.toMatchObject({ status: 401, code: 'unauthorized', requestId: 'req-401' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('reports upload progress as a 0..1 fraction', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const seen: number[] = []
    const p = apiUpload('/api/media/images', new FormData(), (f) => seen.push(f))
    const xhr = FakeXHR.instances.at(-1)
    xhr?.upload.onprogress?.({ lengthComputable: true, loaded: 50, total: 200 } as ProgressEvent)
    xhr?.respond(200, JSON.stringify({ success: true }))
    await p
    expect(seen).toEqual([0.25])
  })

  it('maps a 422 envelope on the XHR path into a validation ApiError', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const p = apiUpload('/api/media/images', new FormData())
    const xhr = FakeXHR.instances.at(-1)
    xhr?.respond(422, JSON.stringify({ success: false, error: 'image too large', code: 'too_large' }))
    await expect(p).rejects.toMatchObject({ status: 422, code: 'validation', message: 'image too large' })
  })
})

describe('apiBinary', () => {
  it('returns the raw bytes on success and carries the cookie + CSRF on a mutation', async () => {
    const mock = stubFetch(new Response(new Uint8Array([1, 2, 3]), { status: 200 }))
    const buf = await apiBinary('/api/functions/test-run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    })
    expect(new Uint8Array(buf)).toEqual(new Uint8Array([1, 2, 3]))
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(init.credentials).toBe('same-origin')
    expect(new Headers(init.headers).get('X-Requested-With')).toBe('picpak')
  })

  it('sends no CSRF header on a safe binary GET (frame download)', async () => {
    const mock = stubFetch(new Response(new Uint8Array([9]), { status: 200 }))
    await apiBinary('/api/media/images/1/frame')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('X-Requested-With')).toBeNull()
  })

  it('parses a {success:false} envelope on a non-2xx into an ApiError', async () => {
    stubFetch(jsonResponse(422, { success: false, error: 'bad frame', code: 'invalid' }, { 'X-Request-ID': 'req-2' }))
    await expect(
      apiBinary('/api/functions/test-run', { method: 'POST', body: '{}' }),
    ).rejects.toMatchObject({ status: 422, code: 'validation', message: 'bad frame', requestId: 'req-2' })
  })

  it('fires the unauthorized teardown on a 401', async () => {
    stubFetch(jsonResponse(401, { success: false, error: 'nope', code: 'unauthorized' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiBinary('/api/media/images/1/frame')).rejects.toMatchObject({ status: 401, code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('maps a fetch rejection to a network ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('down')))
    await expect(apiBinary('/api/media/images/1/frame')).rejects.toMatchObject({ status: 0, code: 'network' })
  })
})
