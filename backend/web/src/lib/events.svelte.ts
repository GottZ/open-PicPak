// Events client (design 19 §4.5): wraps SseClient with typed roster/telemetry/log
// callbacks and owns the TERMINAL-ERROR teardown. The shell ships the transport +
// the auth lifecycle + the device-roster consumer; Docs 21/22/23 own the
// telemetry/log/berry payload schemas.

import { SseClient, type SseStatus } from './sse.svelte'
import { session } from './auth.svelte'
import type { Device } from './api/types'

export type RosterOp = 'upsert' | 'remove'
export interface RosterDelta {
  op: RosterOp
  device: Device
}
export interface RosterSnapshot {
  devices: Device[]
}

export interface EventHandlers {
  /** Full current roster, sent first on every (re)connect. */
  onSnapshot?: (snapshot: RosterSnapshot) => void
  /** One roster change (add/update/remove). */
  onDevices?: (delta: RosterDelta) => void
  /** New telemetry row — payload owned by Doc 22. */
  onTelemetry?: (data: unknown) => void
  /** New log row — payload owned by Doc 21. */
  onLog?: (data: unknown) => void
}

/** Teardown seam — the SAME teardown the 401 interceptor runs (auth.svelte.ts). */
export interface Teardown {
  close: () => void
  invalidate: (reason: string) => void
}

const REVOKED_NOTICE = 'Session ended: the live stream was revoked.'

/**
 * Dispatch one named SSE event. A terminal `event: error` (re-auth failed /
 * refused) is categorically different from a transient drop: the server has
 * refused this key, so reconnecting would hammer it with doomed re-auths (the
 * reconnect-storm a revoked key could trigger). On `error` we therefore (a)
 * close() to suppress the reconnect loop and (b) invalidate() the session →
 * login. Pure + injected teardown so F5 can probe it without a live stream.
 */
export function dispatchEvent(
  name: string,
  data: unknown,
  handlers: EventHandlers,
  teardown: Teardown,
): void {
  switch (name) {
    case 'snapshot':
      handlers.onSnapshot?.(data as RosterSnapshot)
      break
    case 'devices':
      handlers.onDevices?.(data as RosterDelta)
      break
    case 'telemetry':
      handlers.onTelemetry?.(data)
      break
    case 'log':
      handlers.onLog?.(data)
      break
    case 'error':
      teardown.close()
      teardown.invalidate(REVOKED_NOTICE)
      break
  }
}

/** EventsClient binds an SseClient to the live session's Bearer key + the teardown. */
export class EventsClient {
  #sse: SseClient

  constructor(handlers: EventHandlers) {
    this.#sse = new SseClient(
      '/api/events',
      (name, data) =>
        dispatchEvent(name, data, handlers, {
          close: () => this.#sse.close(),
          invalidate: (reason) => session.invalidate(reason),
        }),
      () => (session.key ? { headers: { Authorization: `Bearer ${session.key}` } } : {}),
    )
  }

  /** Live connection status — reactive (drives the ConnIndicator, W5). */
  get status(): SseStatus {
    return this.#sse.status
  }

  connect(): Promise<void> {
    return this.#sse.connect()
  }

  close(): void {
    this.#sse.close()
  }
}
