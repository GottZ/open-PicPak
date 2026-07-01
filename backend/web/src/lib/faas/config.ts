// FaaS config-form logic (Design 25 §4.6 / D25.10) — Policy=Data over the faas_functions row. The pure,
// node-testable core: build/parse the trigger_config JSONB, the egress-allow list, the dither vocabulary,
// the webhook URL, and a client-side cron parse-check. No hardcoded host/secret/serial (air-gap). The
// server re-validates every value (422); this is the pre-flight only.

import type { TriggerType } from './types'

/** Dither is the function's ONLY pack knob (Doc 24 D24.3). `none` = supervisor golden-exact quantize. */
export interface DitherOption {
  value: string
  label: string
}
export const DITHER_OPTIONS: readonly DitherOption[] = [
  { value: 'none', label: 'none (golden-exact)' },
  { value: 'floyd-steinberg', label: 'floyd-steinberg' },
  { value: 'atkinson', label: 'atkinson' },
  { value: 'ordered', label: 'ordered' },
]
export const DEFAULT_DITHER = 'none'

/** Render mode: sync (hot-cache TTL) | prerender (cadence). "" = the 0009 default (sync). */
export const RENDER_MODES = ['sync', 'prerender'] as const
export type RenderMode = (typeof RENDER_MODES)[number]

/** The editable trigger fields the form binds; a superset — only the ones relevant to the type are used. */
export interface TriggerFields {
  mode: RenderMode | ''
  ttlS: number
  intervalS: number
  dither: string
  cron: string
  cadence: string
}

export function emptyTriggerFields(): TriggerFields {
  return { mode: '', ttlS: 0, intervalS: 0, dither: DEFAULT_DITHER, cron: '', cadence: '' }
}

/** Coerce an unknown JSON value to a non-negative integer (0 when absent/invalid) — the form's number inputs. */
function toNonNegInt(v: unknown): number {
  const n = typeof v === 'number' ? v : Number(v)
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0
}

function toStr(v: unknown): string {
  return typeof v === 'string' ? v : ''
}

/** Populate the form from a loaded trigger_config row (the inverse of buildTriggerConfig). */
export function parseTriggerFields(cfg: Record<string, unknown>): TriggerFields {
  const modeRaw = toStr(cfg.mode)
  const mode: RenderMode | '' = modeRaw === 'sync' || modeRaw === 'prerender' ? modeRaw : ''
  const dither = toStr(cfg.dither) || DEFAULT_DITHER
  return {
    mode,
    ttlS: toNonNegInt(cfg.ttl_s),
    intervalS: toNonNegInt(cfg.interval_s),
    dither,
    cron: toStr(cfg.cron),
    cadence: toStr(cfg.cadence),
  }
}

/**
 * Build the trigger_config JSONB to PUT — only the fields relevant to the trigger type, omitting empties
 * so the row stays minimal (the `none` dither is the default → omitted). Matches the shape
 * cmd/admin validTriggerConfig accepts (ttl_s/interval_s ≥ 0, render mode ∈ {"",sync,prerender}).
 */
export function buildTriggerConfig(type: TriggerType, f: TriggerFields): Record<string, unknown> {
  const cfg: Record<string, unknown> = {}
  if (f.dither && f.dither !== DEFAULT_DITHER) cfg.dither = f.dither
  if (type === 'render') {
    if (f.mode) cfg.mode = f.mode
    if (f.ttlS > 0) cfg.ttl_s = f.ttlS
    if (f.intervalS > 0) cfg.interval_s = f.intervalS
    if (f.cadence.trim()) cfg.cadence = f.cadence.trim()
  } else if (type === 'schedule') {
    if (f.cron.trim()) cfg.cron = f.cron.trim()
    if (f.intervalS > 0) cfg.interval_s = f.intervalS
  }
  // webhook carries no cadence config (fired by the inbound hook); dither above still applies.
  return cfg
}

/**
 * Parse an egress-allow editor (one `host[:port]` per line or comma) into a clean, deduped, order-
 * preserving list. Deny-by-default: an empty editor = no outbound network (the server SSRF guard is
 * authoritative, Doc 24 D24.8 — this only shapes the data).
 */
export function parseEgress(text: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of text.split(/[\n,]/)) {
    const host = raw.trim()
    if (host === '' || seen.has(host)) continue
    seen.add(host)
    out.push(host)
  }
  return out
}

/** Render an egress list back into the editor (one host per line). */
export function formatEgress(list: readonly string[]): string {
  return list.join('\n')
}

/**
 * The public inbound webhook URL the operator hands to the caller: `<base>/faas/hook/<name>`, where the
 * base is Doc 22's `webhook_base_url` (the cmd/ingest public origin, D22.13) — NOT the admin origin. Null
 * (hidden) when the base is unset (air-gap: no hostname baked into the SPA).
 */
export function webhookURL(base: string, name: string): string | null {
  const b = base.trim()
  if (b === '') return null
  return `${b.replace(/\/+$/, '')}/faas/hook/${name}`
}

/**
 * A best-effort client-side cron parse-check (D25.10) — exactly 5 whitespace-separated fields. Empty is
 * allowed (the schedule falls back to interval_s in M1). The server / supervisor is authoritative; this
 * only flags an obvious typo before save.
 */
export function validCron(s: string): boolean {
  const t = s.trim()
  if (t === '') return true
  return /^\S+(\s+\S+){4}$/.test(t)
}
