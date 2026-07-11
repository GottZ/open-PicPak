// Localize an EffectDescription to a display string (design/33 §4.3, W-A33.5b). effect-text.ts stays pure
// (it names an i18n key + params, never imports paraglide, so it is node-testable); this thin adapter maps
// a curated key to its message, exactly the discipline C2TracePanel uses inline. svelte-check rejects a
// computed m[...] key, so the map is explicit per EffectKey. A doc/raw description carries its own text.
import { m } from '../../paraglide/messages.js'
import type { EffectDescription, EffectKey } from './effect-text'

const EFFECT_MSG: Record<EffectKey, (p: Record<string, string>) => string> = {
  'sim.effect.set_url': (p) => m['sim.effect.set_url']({ url: p.url }),
  'sim.effect.set_wifi': (p) => m['sim.effect.set_wifi']({ ssid: p.ssid }),
  'sim.effect.wifi_add': (p) => m['sim.effect.wifi_add']({ ssid: p.ssid }),
  'sim.effect.nvs_set': (p) => m['sim.effect.nvs_set']({ ns: p.ns, key: p.key, val: p.val }),
  'sim.effect.net_clear': () => m['sim.effect.net_clear'](),
  'sim.effect.tx_power': (p) => m['sim.effect.tx_power']({ dbm: p.dbm }),
  'sim.effect.reboot': () => m['sim.effect.reboot'](),
  'sim.effect.refresh': () => m['sim.effect.refresh'](),
  'sim.effect.device_sleep': (p) => m['sim.effect.device_sleep']({ s: p.s }),
}

/** The human effect sentence for a described call — curated → its message, else the doc/raw text. */
export function localizeEffect(d: EffectDescription): string {
  return d.source === 'curated' ? EFFECT_MSG[d.key](d.params) : d.text
}
