// Apply-body builders for the "Vorlagen" gallery Verwenden-flow (design/33 §4.5b, W-A33.5b). The gallery
// is the LAIEN entry point, so its apply bodies encode two rules the raw TemplatePicker did not:
//
//   • a render_fn apply is BOUND and ENABLED — target_serials + bind:true + enable:true. Without enable
//     the minted function sits disabled (faasstore Create default), its only toggle in the advanced
//     /functions area — a dead end for a laie. enable:true is the deliberate activation the confirmed
//     click stands for (§4.5b; the matching apply-body delta rides to A30, §9).
//   • the device pick is MANDATORY (probe a): a builder returns null until a target is chosen, and the
//     component derives the confirm-button's disabled state from that null.
//
// Pure + node-testable (form.test.ts sibling): the request body (enable/bind/target_serials) is asserted
// without a DOM. The server stays the apply authority (RequireAdmin on /apply) — this only shapes intent.

import { buildApplyParams } from '../templates/form'
import type { ParamSpec } from '../templates/types'

/** The chosen render_fn target: the one device to bind + activate, plus an optional explicit name. */
export interface RenderFnTarget {
  serial: string | null
  fnName?: string
}

/** The chosen berry_snippet target: one device, or the whole fleet ('*') once explicitly armed. */
export interface BerryTarget {
  mode: 'one' | 'fleet'
  serial: string | null
  armed: boolean
}

/**
 * A render_fn Verwenden body: params + the bound device + bind:true + enable:true. Returns null when no
 * device is picked — the mandatory-step encoding (probe a) the component reads to disable Bestätigen. An
 * empty fnName is omitted (the server derives a stable name); a set one rides as fn_name.
 */
export function buildRenderFnApply(
  specs: ParamSpec[],
  values: Record<string, string>,
  target: RenderFnTarget,
): Record<string, unknown> | null {
  if (!target.serial) return null
  const body: Record<string, unknown> = {
    params: buildApplyParams(specs, values),
    target_serials: [target.serial],
    bind: true,
    enable: true,
  }
  const name = target.fnName?.trim() ?? ''
  if (name !== '') body.fn_name = name
  return body
}

/**
 * A berry_snippet Verwenden body: params + the target set. One device, or ['*'] once the fleet broadcast
 * is armed. Returns null until the mandatory target is chosen (no device, or fleet un-armed) — probe a.
 */
export function buildBerryApply(
  specs: ParamSpec[],
  values: Record<string, string>,
  target: BerryTarget,
): Record<string, unknown> | null {
  let serials: string[]
  if (target.mode === 'fleet') {
    if (!target.armed) return null
    serials = ['*']
  } else {
    if (!target.serial) return null
    serials = [target.serial]
  }
  return { params: buildApplyParams(specs, values), target_serials: serials }
}
