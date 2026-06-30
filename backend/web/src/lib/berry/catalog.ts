// Berry capability catalog (Design 23 §4.2) — the client mirror of the Go-served manifest
// (GET /api/berry/capabilities). The catalog drives BOTH the autocomplete (completion.ts: only
// class != 'forbidden') and the linter (lint.ts: forbidden + severing + unknown classification).
// It is FETCHED, never hardcoded — the Go side is parity-tested against the firmware be_regfunc sites
// (D23.2), so this client stays in sync with the on-device surface by construction.

export type CapClass = 'command' | 'query' | 'intent' | 'store' | 'rtc' | 'dev' | 'forbidden'

export interface Param {
  name: string
  type: string
  /** Trailing optional argument (device_sleep([s])) — rendered bracketed in the signature. */
  opt?: boolean
}

export interface Capability {
  name: string
  params: Param[]
  ret: string
  class: CapClass
  /** 'severing': can cut the device's own C2 path → USB recovery only (D23.8). */
  risk?: string
  /** Advisory minimum firmware; enforced only once running_ver is known (§4.2). */
  min_fw?: string
  doc: string
}

export interface Manifest {
  capabilities: Capability[]
  builtins: string[]
}

// Source: cmd/admin/berry_http.go berryCapabilities → {success, capabilities, builtins}.
export interface CapabilitiesResponse {
  success: true
  capabilities: Capability[]
  builtins: string[]
}

export const FORBIDDEN_CLASS: CapClass = 'forbidden'
export const RISK_SEVERING = 'severing'

/**
 * Human signature for the completion detail + the capability panel:
 *   device_sleep([s]) · wifi_add(ssid, pass[, prio]) · set_url(url)
 * Required params first; optionals folded into one trailing bracket group.
 */
export function signature(c: Capability): string {
  const req = c.params.filter((p) => !p.opt).map((p) => p.name)
  const opt = c.params.filter((p) => p.opt).map((p) => p.name)
  let inner = req.join(', ')
  if (opt.length > 0) inner += (req.length > 0 ? '[, ' : '[') + opt.join(', ') + ']'
  return `${c.name}(${inner})`
}

/** The names the C2 executor actually exposes — the autocomplete + known-call surface. */
export function nonForbidden(m: Manifest): Capability[] {
  return m.capabilities.filter((c) => c.class !== FORBIDDEN_CLASS)
}

/** The render/policy-phase names absent from the C2 VM — the forbidden-lint surface (§2.4). */
export function forbidden(m: Manifest): Capability[] {
  return m.capabilities.filter((c) => c.class === FORBIDDEN_CLASS)
}

/** Index every capability by name for O(1) lint lookup. */
export function byName(m: Manifest): Map<string, Capability> {
  return new Map(m.capabilities.map((c) => [c.name, c]))
}

/** A capability's VM phase, for the forbidden-lint copy (T2 names the real home). */
export function phaseOf(c: Capability): string {
  if (c.doc.startsWith('Render-phase')) return 'render-phase (fb drawing)'
  if (c.doc.startsWith('Policy-phase')) return 'policy-phase (net / Wi-Fi up)'
  return 'a non-C2 phase'
}

// ---- completion surface (kept here, CM6-free, so node tests can assert it without importing the
// CodeMirror editor modules — completion.ts wraps this into the CM6 extension) ----

export interface CompletionOption {
  label: string
  /** signature shown as the completion detail + in the capability panel. */
  detail: string
  /** the capability doc, shown in the completion info panel. */
  info: string
  class: string
  severing: boolean
}

/**
 * The completion surface (T1): every non-forbidden capability + the enabled builtins/keywords. The
 * §2.4 forbidden names and the Doc-13b-prose divergences (sleep/info/set_wifi_add) are NEVER offered —
 * the operator cannot tab-complete a name the executor lacks.
 */
export function completionOptions(manifest: Manifest): CompletionOption[] {
  const caps: CompletionOption[] = nonForbidden(manifest).map((c) => ({
    label: c.name,
    detail: signature(c),
    info: c.doc,
    class: c.class,
    severing: c.risk === RISK_SEVERING,
  }))
  const builtins: CompletionOption[] = manifest.builtins.map((b) => ({
    label: b,
    detail: 'builtin',
    info: 'Berry builtin / keyword',
    class: 'builtin',
    severing: false,
  }))
  return [...caps, ...builtins]
}
