// Effect language for the C2 trace + gallery cards (design/33 §4.3/§4.5b, W-A33.4). The raw trace lines
// ({fn, args, ret}) speak developer Berry; this table translates the EFFECT-CARRYING calls (the
// command + intent classes) into a human sentence the mid-tier operator reads — "set_wifi(\"a\",\"b\")"
// becomes "changes the Wi-Fi credentials … (severing)". Consumed by both the Trace-UI and the gallery
// card summary (which classifies from the STATIC calls() pass, so it may have a name with no args).
//
// Resolution order (design §4.3): (1) a curated entry → a localized i18n key + params; (2) else the
// catalog `doc` for that capability, shown verbatim; (3) else neutral raw text `fn()`. An UNKNOWN
// function never throws — it returns {source:'raw'} (§7 probe d). i18n stays correct because the curated
// text lives in messages/{en,de}.json under `sim.effect.*`; this module only names the key + params, so
// it stays pure and node-testable (no paraglide import, no {@html}).

import { type Arg, formatArg } from './c2-surface'
import { byName, type Manifest, RISK_SEVERING } from '../berry/catalog'

export type EffectKey =
  | 'sim.effect.set_url'
  | 'sim.effect.set_wifi'
  | 'sim.effect.wifi_add'
  | 'sim.effect.nvs_set'
  | 'sim.effect.net_clear'
  | 'sim.effect.tx_power'
  | 'sim.effect.reboot'
  | 'sim.effect.refresh'
  | 'sim.effect.device_sleep'

export type EffectDescription =
  | { source: 'curated'; key: EffectKey; params: Record<string, string>; severing: boolean }
  | { source: 'doc'; text: string; severing: boolean }
  | { source: 'raw'; text: string; severing: false }

/** The severing capabilities (catalog risk mirror) — used when no manifest is supplied to describeEffect. */
const SEVERING = new Set(['set_url', 'set_wifi', 'nvs_set', 'net_clear'])

/** A placeholder for a param whose argument the caller did not supply (gallery cards have names, no args). */
const ANY = '…'

/** The inner text of a string arg, else its Berry rendering, else the placeholder. */
function argText(args: Arg[] | undefined, i: number): string {
  const a = args?.[i]
  if (a == null) return ANY
  return a.kind === 'string' ? a.value : formatArg(a)
}

type Curated = { key: EffectKey; params: (args?: Arg[]) => Record<string, string> }

const CURATED: Record<string, Curated> = {
  set_url: { key: 'sim.effect.set_url', params: (a) => ({ url: argText(a, 0) }) },
  set_wifi: { key: 'sim.effect.set_wifi', params: (a) => ({ ssid: argText(a, 0) }) },
  wifi_add: { key: 'sim.effect.wifi_add', params: (a) => ({ ssid: argText(a, 0) }) },
  nvs_set: {
    key: 'sim.effect.nvs_set',
    params: (a) => ({ ns: argText(a, 0), key: argText(a, 1), val: argText(a, 2) }),
  },
  net_clear: { key: 'sim.effect.net_clear', params: () => ({}) },
  tx_power: { key: 'sim.effect.tx_power', params: (a) => ({ dbm: argText(a, 0) }) },
  reboot: { key: 'sim.effect.reboot', params: () => ({}) },
  refresh: { key: 'sim.effect.refresh', params: () => ({}) },
  device_sleep: { key: 'sim.effect.device_sleep', params: (a) => ({ s: argText(a, 0) }) },
}

/**
 * Describe a call's effect. `args` is optional (a gallery card summarizing a static call has none).
 * `manifest` supplies the catalog `doc` fallback + the authoritative severing flag; without it a small
 * built-in severing set is used. Total: an unknown function yields {source:'raw'} rather than throwing.
 */
export function describeEffect(fn: string, args?: Arg[], manifest?: Manifest): EffectDescription {
  const cap = manifest ? byName(manifest).get(fn) : undefined
  const severing = cap ? cap.risk === RISK_SEVERING : SEVERING.has(fn)
  const curated = CURATED[fn]
  if (curated) {
    return { source: 'curated', key: curated.key, params: curated.params(args), severing }
  }
  if (cap && cap.doc) {
    return { source: 'doc', text: cap.doc, severing }
  }
  return { source: 'raw', text: `${fn}()`, severing: false }
}
