// FaaS capability catalog (Design 25 §4.5 / D25.14) — the curated worker scope the editor documents +
// autocompletes. UNLIKE Berry's catalog, this is a STATIC client-side list, NOT a server-fetched
// parity-tested manifest: the worker runtime is our own TS (Doc 24), not firmware, so there is no
// be_regfunc ground truth to diff (§4.5). It is documentation of the one authoring contract (D25.14):
//   export default async (ctx, cap) => ({ image, dither?, next_wake_hint? })
// with cap = { fetch, sharp, secrets, log } and ctx = { serial, channel, trigger, now (+ payload) }.
// CM6-free so the completion surface is node-testable (T7); completion.ts wraps it into the CM6 extension.

export interface CapEntry {
  /** completion label — the full dotted path the operator types (e.g. `cap.fetch`). */
  label: string
  /** signature shown as the completion detail + in the hint panel. */
  detail: string
  /** doc, shown in the completion info panel + the hint panel. */
  info: string
}

/** The curated capabilities (cap.*) — the ONLY host surface the worker exposes (Doc 24 D24.7). */
export const CAP_ENTRIES: readonly CapEntry[] = [
  {
    label: 'cap.fetch',
    detail: 'cap.fetch(url, opts?) → Response',
    info: 'Egress-guarded fetch. Only hosts in this function’s egress-allow list resolve; everything else (and private/metadata IPs) is denied server-side. await .arrayBuffer() / .json().',
  },
  {
    label: 'cap.sharp',
    detail: 'cap.sharp(input?) → Sharp',
    info: 'libvips image pipeline (resize / composite / text / raw). Return the result as { image: await img.png() } (or a raw buffer) — the supervisor packs it to a 400×300 BWRY frame.',
  },
  {
    label: 'cap.secrets',
    detail: 'cap.secrets.<bound> → string',
    info: 'A bound secret’s value. Least privilege: only the names in this function’s secret-bindings resolve; an unbound name is undefined. In test-run, values are stubbed by default (D25.12).',
  },
  {
    label: 'cap.log',
    detail: 'cap.log(level, msg)',
    info: 'Emit a structured worker log line (level: info | warn | error). Assembled into the render/test-run log[]. This is a CAPABILITY, not a return field (D25.14).',
  },
]

// The server-resolved call context (ctx.*). NOTE the trigger + payload live under `ctx.trigger` — the
// worker exposes `ctx.trigger = { type, payload? }` (runtime.ts), NOT a top-level `ctx.payload` (correcting
// the design doc's stated shape; the running worker is the truth).
export const CTX_ENTRIES: readonly CapEntry[] = [
  { label: 'ctx.serial', detail: 'ctx.serial → string', info: 'The device this render is for.' },
  { label: 'ctx.channel', detail: 'ctx.channel → string', info: 'The device’s channel, resolved server-side (never a client value).' },
  { label: 'ctx.trigger', detail: 'ctx.trigger → { type, payload? }', info: 'The trigger object — { type, payload }.' },
  { label: 'ctx.trigger.type', detail: 'ctx.trigger.type → "render"|"schedule"|"webhook"', info: 'Which trigger fired this run.' },
  { label: 'ctx.trigger.payload', detail: 'ctx.trigger.payload → unknown', info: 'The webhook JSON body (webhook trigger only; undefined otherwise).' },
  { label: 'ctx.now', detail: 'ctx.now → string', info: 'Server clock (RFC3339) — drives day/night wake selection.' },
]

/** The one return contract (D25.14). image is required; dither/next_wake_hint are optional. */
export const RETURN_ENTRIES: readonly CapEntry[] = [
  { label: 'image', detail: 'image (required)', info: 'The rendered image (a PNG/raw buffer from cap.sharp). The supervisor fits + quantizes it to BWRY.' },
  { label: 'dither', detail: 'dither? (optional)', info: 'Override the pack dither for this run: none | floyd-steinberg | atkinson | ordered.' },
  { label: 'next_wake_hint', detail: 'next_wake_hint? (optional)', info: 'Suggested seconds until the device’s next poll.' },
]

/** The authoring template (D25.14 signature) — a new function is scaffolded to exactly this shape so it
 * matches what the Doc 24 worker invokes. Air-gap: no real host / serial / secret — only panel dims. */
export const TEMPLATE = `export default async (ctx, cap) => {
  // Author a render function. Return a 400x300 image; the supervisor packs it to BWRY.
  // Scope: cap.fetch(url), cap.sharp(...), cap.secrets.<bound>, cap.log(level, msg).
  const img = cap.sharp({ create: { width: 400, height: 300, channels: 3, background: '#ffffff' } })
  return { image: await img.png() }
}
`

/**
 * The secret entries — `cap.secrets.<name>` for EACH bound name, and NONE else (D25.7 / T7). The operator
 * cannot tab-complete a secret the worker will not resolve; binding a name is what makes it completable.
 */
export function secretEntries(boundSecrets: readonly string[]): CapEntry[] {
  return boundSecrets.map((name) => ({
    label: `cap.secrets.${name}`,
    detail: `cap.secrets.${name} → string`,
    info: 'A bound secret value (least privilege). Stubbed in test-run by default (D25.12).',
  }))
}

/**
 * The full completion surface for a function bound to `boundSecrets`: the static cap.* + ctx.* + the
 * bound secret paths + the bare `cap`/`ctx` roots. NO unbound secret name is ever offered (T7).
 */
export function completionOptions(boundSecrets: readonly string[]): CapEntry[] {
  const roots: CapEntry[] = [
    { label: 'cap', detail: 'cap = { fetch, sharp, secrets, log }', info: 'The curated worker capabilities.' },
    { label: 'ctx', detail: 'ctx = { serial, channel, trigger, now, payload? }', info: 'The server-resolved call context.' },
  ]
  return [...roots, ...CAP_ENTRIES, ...secretEntries(boundSecrets), ...CTX_ENTRIES]
}
