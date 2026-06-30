// SSE client (design 19 §4.5). `.svelte.ts` enables runes in a module so
// `status` is reactive in consuming components (the ConnIndicator, W5).
//
// fetch + ReadableStream + eventsource-parser, NOT native EventSource: the
// latter cannot send an Authorization header (admin auth is Bearer; `?token=`
// would land in proxy logs) and is GET-only. This client streams named events
// (snapshot | devices | telemetry | log | error) to an onEvent dispatcher and
// reconnects with exponential backoff + jitter (cap 30s) after any non-clean
// end, until close() is called. The status field drives a page's poll fallback:
// callers poll while it is not 'open'.

import { createParser, type EventSourceMessage } from 'eventsource-parser'

export type SseStatus = 'idle' | 'connecting' | 'open' | 'closed' | 'error'

export class SseClient {
  status = $state<SseStatus>('idle')

  #ctrl: AbortController | null = null
  #timer: ReturnType<typeof setTimeout> | null = null
  #retryMs = 1_000
  #stopped = false

  constructor(
    private readonly url: string,
    private readonly onEvent: (name: string, data: unknown) => void,
    private readonly getInit: () => RequestInit = () => ({}),
  ) {}

  async connect(): Promise<void> {
    this.#stopped = false
    this.#ctrl?.abort()
    this.#ctrl = new AbortController()
    this.status = 'connecting'
    try {
      const init = this.getInit()
      const res = await fetch(this.url, {
        ...init,
        signal: this.#ctrl.signal,
        headers: { Accept: 'text/event-stream', ...init.headers },
      })
      // A 429 (connection cap) or 401 (revoked key) lands here as !ok → 'error' →
      // the page polls; a freed slot / re-login recovers on retry.
      if (!res.ok || !res.body) throw new Error(`sse http ${res.status}`)
      this.status = 'open'
      this.#retryMs = 1_000 // reset backoff after a good connect

      const parser = createParser({
        onEvent: (ev: EventSourceMessage) => {
          let data: unknown = null
          try {
            data = ev.data ? JSON.parse(ev.data) : null
          } catch {
            return // a malformed frame is dropped, never crashes the stream
          }
          this.onEvent(ev.event ?? 'message', data)
        },
      })
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        parser.feed(value)
      }
      // Clean EOF: the server ended the stream (shutdown). Reconnect so a
      // restarted server re-establishes. A terminal `event: error` (revocation)
      // is handled by events.svelte.ts, which calls close() BEFORE this EOF lands
      // → #stopped is set → #scheduleReconnect does not reconnect (no storm).
      this.#scheduleReconnect()
    } catch {
      if (this.#ctrl?.signal.aborted || this.#stopped) {
        this.status = 'closed'
        return
      }
      this.status = 'error'
      this.#scheduleReconnect()
    }
  }

  #scheduleReconnect(): void {
    if (this.#stopped) {
      this.status = 'closed'
      return
    }
    const wait = this.#retryMs * (0.5 + Math.random())
    this.#retryMs = Math.min(this.#retryMs * 2, 30_000)
    this.#timer = setTimeout(() => {
      if (!this.#stopped) void this.connect()
    }, wait)
  }

  close(): void {
    this.#stopped = true
    if (this.#timer) {
      clearTimeout(this.#timer)
      this.#timer = null
    }
    this.#ctrl?.abort()
    this.status = 'closed'
  }
}
