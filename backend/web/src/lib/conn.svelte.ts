// Shell-wide live-connection status (design 19 D19.14, K13). The active page's
// EventsClient mirrors its SseClient status here (FleetDashboard syncs it via a
// $effect); App.svelte renders ConnIndicator off it, so an operator always knows
// when the live views are stale because the stream dropped. A22's "reconnecting
// — live view paused" banner reuses THIS signal rather than inventing its own.

import type { SseStatus } from './sse.svelte'
import { m } from '../paraglide/messages.js'

class ConnState {
  status = $state<SseStatus>('idle')
}

export const conn = new ConnState()

export type ConnTone = 'live' | 'reconnecting' | 'offline'

/** Map the raw SSE status to the shell indicator's three display states. */
export function connDisplay(s: SseStatus): { label: string; tone: ConnTone } {
  switch (s) {
    case 'open':
      return { label: m['conn.live'](), tone: 'live' }
    case 'connecting':
    case 'error':
      // 'error' is a transient connect failure that the client retries with
      // backoff — to the operator that is "reconnecting", not "offline".
      return { label: m['conn.reconnecting'](), tone: 'reconnecting' }
    default:
      return { label: m['conn.offline'](), tone: 'offline' } // idle | closed
  }
}
