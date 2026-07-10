// Berry hover documentation (Design 33 §4.1.1) — pure, manifest-driven. Given the bare identifier under
// the cursor, resolve it against the served catalog and compose the tooltip content: signature + doc +
// a severing warning + a phase hint for the forbidden render/policy names. No new dependency, CM6-free
// and node-testable (the ONE place that turns this into a hoverTooltip DOM node is editor.ts).
//
// Only catalog capabilities get a tooltip — builtins, keywords and user identifiers resolve to null (the
// manifest is the single source, exactly as completion.ts and lint.ts treat it).

import { type Manifest, byName, signature, phaseOf, RISK_SEVERING, FORBIDDEN_CLASS } from './catalog'

export interface HoverDoc {
  /** the capability name this word resolves to */
  name: string
  /** human signature line, e.g. set_url(url) */
  signature: string
  /** the capability doc prose */
  doc: string
  /** severing capability — a call can cut the device's own C2 path (USB recovery only) */
  severing: boolean
  /** for forbidden names: the real VM phase (render/policy); null for the C2 surface */
  phase: string | null
  /** the composed tooltip lines in render order: signature, doc, then any warnings */
  lines: string[]
}

const SEVERING_LINE = "⚠ severing — a call can cut the device's own C2 path (USB recovery only)."

/**
 * Resolve a bare identifier to its hover documentation, or null when the word is not a catalog
 * capability. The composed `lines` are what the tooltip renders (as text nodes only — D19.10).
 */
export function hoverDoc(word: string, manifest: Manifest): HoverDoc | null {
  const cap = byName(manifest).get(word)
  if (!cap) return null

  const severing = cap.risk === RISK_SEVERING
  const phase = cap.class === FORBIDDEN_CLASS ? phaseOf(cap) : null

  const lines = [signature(cap), cap.doc]
  if (severing) lines.push(SEVERING_LINE)
  if (phase) {
    lines.push(
      `⚠ ${phase} capability — not in the C2 executor; it faults at runtime and the cursor still advances.`,
    )
  }

  return { name: cap.name, signature: signature(cap), doc: cap.doc, severing, phase, lines }
}
