// F5 — terminal-error teardown (design 19 §6, negatively probed): a terminal
// `event: error` must drive BOTH SseClient.close() (suppress the reconnect loop —
// no doomed-re-auth storm after revocation) AND session.invalidate() (operator →
// login). A transient drop is categorically different and reconnects. Also pins
// the typed payload dispatch. Pure node; teardown is injected.

import { describe, expect, it, vi } from 'vitest'
import { dispatchEvent, type RosterDelta, type RosterSnapshot } from './events.svelte'

function spies() {
  return { close: vi.fn(), invalidate: vi.fn() }
}

describe('dispatchEvent', () => {
  it('tears down on a terminal error event: close() AND invalidate()', () => {
    const t = spies()
    dispatchEvent('error', { code: 'revoked' }, {}, t)
    expect(t.close).toHaveBeenCalledTimes(1)
    expect(t.invalidate).toHaveBeenCalledTimes(1)
    // The reason text reaches the login screen notice.
    // A34.2: the revoked notice resolves through the catalog; node → baseLocale (de).
    expect(t.invalidate.mock.calls[0][0]).toMatch(/widerrufen/i)
  })

  it('does NOT tear down on a roster delta (only error is terminal)', () => {
    const t = spies()
    const delta: RosterDelta = { op: 'upsert', device: { serial: 'D2XXXXR', label: null, channel: 'stable', last_seen: null, bonded: false } }
    const onDevices = vi.fn()
    dispatchEvent('devices', delta, { onDevices }, t)
    expect(onDevices).toHaveBeenCalledWith(delta)
    expect(t.close).not.toHaveBeenCalled()
    expect(t.invalidate).not.toHaveBeenCalled()
  })

  it('routes snapshot to onSnapshot', () => {
    const t = spies()
    const snap: RosterSnapshot = { devices: [] }
    const onSnapshot = vi.fn()
    dispatchEvent('snapshot', snap, { onSnapshot }, t)
    expect(onSnapshot).toHaveBeenCalledWith(snap)
  })

  it('routes telemetry/log to their handlers (payload owned by Docs 21/22)', () => {
    const t = spies()
    const onTelemetry = vi.fn()
    const onLog = vi.fn()
    dispatchEvent('telemetry', { v: 1 }, { onTelemetry }, t)
    dispatchEvent('log', { line: 'x' }, { onLog }, t)
    expect(onTelemetry).toHaveBeenCalledWith({ v: 1 })
    expect(onLog).toHaveBeenCalledWith({ line: 'x' })
  })

  it('ignores an unknown event name without tearing down', () => {
    const t = spies()
    dispatchEvent('mystery', {}, {}, t)
    expect(t.close).not.toHaveBeenCalled()
    expect(t.invalidate).not.toHaveBeenCalled()
  })
})
