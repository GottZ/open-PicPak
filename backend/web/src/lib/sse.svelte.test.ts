// F3 — SseClient gates (design 19 §6): named-event dispatch off a fetch stream,
// the Authorization header, the HTTP-error → 'error' status (poll-fallback
// trigger), malformed-frame tolerance, and close() suppressing reconnect.
// fetch + ReadableStream are stubbed; pure node.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { SseClient } from './sse.svelte'

// sseResponse builds a 200 text/event-stream Response whose body is the given
// SSE frames as one chunk (an explicit ReadableStream so .body is never null).
function sseResponse(...frames: string[]): Response {
  const bytes = new TextEncoder().encode(frames.join(''))
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(bytes)
      c.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('SseClient', () => {
  it('dispatches named events with parsed JSON data', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse(
          'event: snapshot\ndata: {"devices":[]}\n\n',
          ': ping\n\n',
          'event: devices\ndata: {"op":"upsert","device":{"serial":"D2XXXXR"}}\n\n',
        ),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close() // stop the post-EOF reconnect timer

    expect(got).toEqual([
      ['snapshot', { devices: [] }],
      ['devices', { op: 'upsert', device: { serial: 'D2XXXXR' } }],
    ])
  })

  it('sends the Authorization header from getInit and Accept: text/event-stream', async () => {
    const key = ['cafe', 'f00d'].join('') // doc-value, assembled at runtime (repo rule)
    const fetchMock = vi.fn().mockResolvedValueOnce(sseResponse('event: snapshot\ndata: {}\n\n'))
    vi.stubGlobal('fetch', fetchMock)
    const sse = new SseClient(
      '/api/events',
      () => {},
      () => ({ headers: { Authorization: `Bearer ${key}` } }),
    )
    await sse.connect()
    sse.close()

    const init = fetchMock.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBe(`Bearer ${key}`)
    expect(headers.Accept).toBe('text/event-stream')
  })

  it('goes to error on a non-ok status (429 connection cap → poll fallback)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(null, { status: 429 })))
    const sse = new SseClient('/api/events', () => {})
    await sse.connect()
    expect(sse.status).toBe('error')
    sse.close() // stop the backoff reconnect timer
  })

  it('drops a malformed data frame without crashing the stream', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse('event: snapshot\ndata: {bad json\n\n', 'event: devices\ndata: {"op":"remove"}\n\n'),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close()

    // The malformed snapshot frame is skipped; the valid devices frame survives.
    expect(got).toEqual([['devices', { op: 'remove' }]])
  })

  it('close() before connect suppresses any reconnect (status stays closed)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(sseResponse('event: snapshot\ndata: {}\n\n')))
    const sse = new SseClient('/api/events', () => {})
    await sse.connect() // clean EOF would schedule a reconnect
    sse.close()
    expect(sse.status).toBe('closed')
    // A reconnect would have re-entered 'connecting'; closed stays closed.
    await new Promise((r) => setTimeout(r, 5))
    expect(sse.status).toBe('closed')
  })
})
