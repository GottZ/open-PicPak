// Berry enqueue logic (Design 23 §4.4 / D23.6 / D23.8) — the pure, node-testable core of the RCE-shipping
// wave (W3). The actual POST goes through Doc 17 §4.5 verbatim (cmd/admin/commands.go); this module only
// decides path/body, whether the action is armed, the blast radius, and the risk level. The UI in
// BerryEditor.svelte wires these to the button + the typed-'*' confirm + the server's requireAdmin 403.

import type { Finding } from './lint'
import type { Device } from '../api/types'

// Mirror of commandstore.ScriptMax / firmware C2_SCRIPT_MAX. The server 422s past it; this gives the
// editor inline feedback before the round-trip.
export const SCRIPT_MAX = 16384

/** The arming token an operator must type to broadcast to the whole fleet (D23.6). */
export const FLEET_ARM_TOKEN = '*'

export type Target = { kind: 'one'; serial: string } | { kind: 'fleet' }

export interface EnqueueRequest {
  path: string
  body: string
}

/**
 * Path + JSON body for an enqueue (Doc 17 §4.5): a single device hits the path-serial route, the fleet
 * hits POST /api/command with serial:'*'. note is omitted when blank so the column stays NULL.
 */
export function buildEnqueue(target: Target, script: string, note: string): EnqueueRequest {
  const trimmedNote = note.trim()
  const noteField = trimmedNote === '' ? undefined : trimmedNote
  if (target.kind === 'fleet') {
    return { path: '/api/command', body: JSON.stringify({ serial: FLEET_ARM_TOKEN, script, note: noteField }) }
  }
  return {
    path: `/api/devices/${encodeURIComponent(target.serial)}/command`,
    body: JSON.stringify({ script, note: noteField }),
  }
}

/** Whether the script length is enqueue-eligible (non-empty, within the cap). */
export function scriptOk(script: string): boolean {
  const len = script.length
  return script.trim().length > 0 && len <= SCRIPT_MAX
}

/**
 * Is the Enqueue action allowed to fire? (T5/T7) Server-authoritative requireAdmin still 403s a
 * hand-crafted POST; this is the cosmetic gate. A fleet broadcast is armed ONLY when the operator typed
 * exactly '*' (D23.6 — one mis-click cannot broadcast RCE to the whole fleet). A single device needs a
 * selection. Non-admin or an empty/oversized script → never.
 */
export function canEnqueue(
  target: Target | null,
  typedArm: string,
  isAdmin: boolean,
  script: string,
): boolean {
  if (!isAdmin || !target || !scriptOk(script)) return false
  if (target.kind === 'fleet') return typedArm.trim() === FLEET_ARM_TOKEN
  return target.serial !== ''
}

/**
 * The snapshot blast radius for a '*' enqueue: the current roster size (D23.6 — a device onboarded AFTER
 * the enqueue seeds its cursor to HEAD and so will NOT receive it; the snapshot membership is the
 * truthful target set, and the queue is append-only / no-recall).
 */
export function blastRadius(devices: Device[]): number {
  return devices.length
}

export type RiskLevel = 'normal' | 'severing' | 'fleet-severing'

/**
 * Highest-risk classification for the enqueue panel (T8/D23.8). A severing capability (set_url/set_wifi/
 * nvs_set/net_clear) can cut the device's own C2 path → USB recovery only; the SAME under '*' can strand
 * the WHOLE fleet at once → the top banner. It warns, never forbids (operator authority is real).
 */
export function riskLevel(findings: Finding[], target: Target | null): RiskLevel {
  const severing = findings.some((f) => f.kind === 'severing')
  if (!severing) return 'normal'
  return target?.kind === 'fleet' ? 'fleet-severing' : 'severing'
}
