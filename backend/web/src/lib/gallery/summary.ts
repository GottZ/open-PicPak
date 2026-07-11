// Static snippet summaries for the gallery cards + the fleet-'*' confirm (design/33 §4.5b/§5 S8,
// W-A33.5b). A berry_snippet card shows what the snippet DOES without running it, and the fleet-'*'
// confirm must warn about severing calls — BOTH from the STATIC source, never a trace (S8: a severing call
// hidden behind a branch that is only true on-device stays invisible to the deceivable dynamic trace; the
// static pass is branch-independent). This module extracts the distinct capability calls from a source
// (mirroring lint.ts calls() extraction) and hands each to describeEffect (effect-text.ts). Pure +
// node-testable: no CM6, no paraglide import (the Svelte card maps the returned EffectKeys to messages,
// exactly like C2TracePanel), no {@html}.

import { stripCommentsAndStrings } from '../berry/lint'
import { byName, type Manifest, RISK_SEVERING } from '../berry/catalog'
import { describeEffect, type EffectDescription } from '../sim/effect-text'

// Same extraction primitives as lint.ts calls(): a bare `name(` call, and a `def name` user definition.
const CALL_RE = /([A-Za-z_]\w*)\s*\(/g
const DEF_RE = /\bdef\s+([A-Za-z_]\w*)/g

/**
 * The distinct catalog capabilities called in a snippet source, in first-appearance order. Mirrors
 * lint.ts calls(): strip comments/strings first, skip member calls (obj.method — not a bare global),
 * builtins and user-def'd names; keep only names the capability catalog knows (those carry an effect).
 */
export function calledCapabilities(source: string, manifest: Manifest): string[] {
  const { code } = stripCommentsAndStrings(source)
  const known = byName(manifest)
  const builtins = new Set(manifest.builtins)
  const userDefs = new Set<string>()
  for (const m of code.matchAll(DEF_RE)) userDefs.add(m[1])

  const out: string[] = []
  const seen = new Set<string>()
  for (const m of code.matchAll(CALL_RE)) {
    const name = m[1]
    const at = m.index ?? 0
    if (code.slice(0, at).trimEnd().endsWith('.')) continue // obj.method(...) is not a bare capability
    if (builtins.has(name) || userDefs.has(name) || seen.has(name)) continue
    if (!known.has(name)) continue // only catalog capabilities carry a described effect
    seen.add(name)
    out.push(name)
  }
  return out
}

/** One effect description per distinct capability call — the card summary body (localized in the card). */
export function effectSummary(source: string, manifest: Manifest): EffectDescription[] {
  return calledCapabilities(source, manifest).map((n) => describeEffect(n, undefined, manifest))
}

/**
 * The distinct SEVERING capabilities a snippet calls — the S8 static warning source for the fleet-'*'
 * confirm. Branch-independent by construction (calledCapabilities scans the whole stripped source), so a
 * severing call buried in a never-taken sim branch is still reported.
 */
export function severingCalls(source: string, manifest: Manifest): string[] {
  const known = byName(manifest)
  return calledCapabilities(source, manifest).filter((n) => known.get(n)?.risk === RISK_SEVERING)
}
