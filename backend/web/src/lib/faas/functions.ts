// FaaS editor pure logic (Design 25, W1) — the node-testable core of the /functions page: the create-form
// validation (mirroring the server's ValidName so a bad name is caught before the round-trip), the trigger
// vocabulary, and the blast-radius label. No DOM/Svelte imports. The server stays authoritative (422/409);
// this is the pre-flight, never the gate.

import type { TriggerType } from './types'
import { m } from '../../paraglide/messages.js'

/** The trigger vocabulary (0009 CHECK) — the create form's radio/select options, in author order. */
export const TRIGGER_TYPES: readonly TriggerType[] = ['render', 'schedule', 'webhook']

/**
 * Function-name charset — the EXACT mirror of faasstore.nameRe (`^[a-z0-9][a-z0-9._-]{0,127}$`): a
 * lowercase-leading, url/JSON-safe slug, ≤128 chars. Kept in sync with the server (the server 422s a
 * mismatch regardless — this only shortens the loop).
 */
export const NAME_RE = /^[a-z0-9][a-z0-9._-]{0,127}$/

export function validName(name: string): boolean {
  return NAME_RE.test(name)
}

export interface NewFunctionDraft {
  name: string
  source: string
  triggerType: TriggerType
}

/**
 * Validate a create draft; returns the first blocking error message, or null when it is submittable. The
 * order matches the server's checks (name → source) so the surfaced message is the one the server would
 * also return. Source is required (an empty function has nothing to run); trigger_type is constrained by
 * the select, so it is only re-checked defensively.
 */
export function newFunctionError(draft: NewFunctionDraft): string | null {
  const name = draft.name.trim()
  if (name === '') return m['faas.err.name_required']()
  if (!validName(name)) return m['faas.err.name_charset']()
  if (draft.source.trim() === '') return m['faas.err.source_required']()
  if (!TRIGGER_TYPES.includes(draft.triggerType)) return m['faas.err.trigger_type']()
  return null
}

/**
 * Human blast-radius label for a bound-device count (D25.9): how many devices a source edit / a delete
 * will affect. Pluralized; zero is spelled out so an unbound function reads clearly.
 */
export function blastRadiusLabel(count: number): string {
  if (count <= 0) return m['faas.blast_none']()
  if (count === 1) return m['faas.blast_one']()
  return m['faas.blast_many']({ count })
}
