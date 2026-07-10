// Events client (design 19 §4.5): wraps SseClient with typed roster/telemetry/log
// callbacks and owns the TERMINAL-ERROR teardown. The shell ships the transport +
// the auth lifecycle + the device-roster consumer; Docs 21/22/23 own the
// telemetry/log/berry payload schemas.

import { SseClient, type SseStatus } from './sse.svelte'
import { session } from './auth.svelte'
import type { Device } from './api/types'
import { m } from '../paraglide/messages.js'

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
  /** A device's C2 cursor advanced — payload owned by Doc 23 (the Berry feedback model). */
  onC2Cursor?: (data: unknown) => void
}

/** Teardown seam — the SAME teardown the 401 interceptor runs (auth.svelte.ts). */
export interface Teardown {
  close: () => void
  invalidate: (reason: string) => void
}

// Resolved per-call (not a module-const) so the active locale — set at boot,
// before this lazily-imported module loads — is always the one in effect.
const revokedNotice = (): string => m['app.session_revoked']()

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
    case 'c2cursor':
      handlers.onC2Cursor?.(data)
      break
    case 'error':
      teardown.close()
      teardown.invalidate(revokedNotice())
      break
  }
}

/** EventsClient binds an SseClient to the live cookie session + the teardown. */
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
      // The httpOnly ppk_sid cookie authenticates the stream — credentials: 'same-origin' rides it
      // along (design 28 §4.3). A GET, so no CSRF header; a revoked cookie 401s → terminal error.
      () => ({ credentials: 'same-origin' }),
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
