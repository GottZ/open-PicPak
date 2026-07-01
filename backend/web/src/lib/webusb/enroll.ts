// C2 enrollment (Design 26 §4.2 step 5 / D26.5, D26.7, D26.8, D17.4). After BOND, the SPA registers the
// device's PUBLIC point via Doc 17 POST /api/devices. The pure decisions live here (body shape, success
// classification, the admin/collision gates); EnrollStep.svelte wires them to apiFetch + the affordance.

import { pubkeyHexToBase64 } from './pubkey'
import type { Device } from '../api/types'

export interface EnrollInput {
  /** the BARE inner <S> (= devices.serial, = ?sn=<S>), NOT the JSON blob (D26.5). */
  serial: string
  /** the pubkey hex emitted by C2 BOND. */
  pubkeyHex: string
  /** only if the operator set it this session (COH34 omit-to-preserve). */
  label?: string
  /** only if the operator changed it this session (COH34). */
  channel?: string
}

export interface EnrollBody {
  serial: string
  ecdsa_pubkey: string
  label?: string
  channel?: string
}

/**
 * Build the POST /api/devices body (Doc 17 §4.4). serial = the bare inner <S> (D26.5, NOT the JSON blob);
 * pubkey hex → base64 with the D26.7 65-byte/0x04 pre-check (throws on a bad key so no round-trip 422).
 * label/channel are OMITTED unless the operator set them this session (COH34): Doc 17's COALESCE preserves
 * an operator-tuned channel (e.g. 'beta') only when the field is ABSENT — always sending a default 'stable'
 * would silently re-target OTA off the device's channel on every re-enroll. New devices take the table default.
 */
export function buildEnrollBody(input: EnrollInput): EnrollBody {
  const body: EnrollBody = { serial: input.serial, ecdsa_pubkey: pubkeyHexToBase64(input.pubkeyHex) }
  if (input.label !== undefined && input.label !== '') body.label = input.label
  if (input.channel !== undefined && input.channel !== '') body.channel = input.channel
  return body
}

/**
 * POST /api/devices reply action: created | rebonded | updated are ALL success (D17.4/F5). An idempotent
 * re-enroll of an already-bonded device returns 'updated' (no session churn) — treating only 'created' as
 * success would read a legitimate re-enroll as a failure.
 */
export function isEnrollSuccess(action: string): boolean {
  return action === 'created' || action === 'rebonded' || action === 'updated'
}

/**
 * Enroll affordance gate (D26.8/F6). The POST needs admin (the server 403s a hand-crafted POST regardless via
 * requireAdmin); the local steps (connect/backup/flash/provision/restore) need only a valid operator login,
 * since physical possession flashes the device regardless of our UI. A pubkey must exist to POST.
 */
export function canEnroll(isAdmin: boolean, hasPubkey: boolean): boolean {
  return isAdmin && hasPubkey
}

/**
 * Serial-collision pre-check (Design 26 G9): GET /api/devices/{serial} before enroll. A serial already
 * registered to a (possibly different) physical device must NOT be silently re-keyed — the changed-pubkey
 * path re-bonds and drops the deployed device off the fleet, shown as 'rebonded' success. The Device wire
 * shape carries no pubkey (T11), so the client cannot distinguish a same-device re-enroll from a true
 * collision; it conservatively requires operator confirmation whenever the serial already exists.
 */
export function enrollNeedsConfirm(existing: Device | null): boolean {
  return existing !== null
}
