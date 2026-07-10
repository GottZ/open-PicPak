// Param-form logic for the template picker (A30 W7) — pure, node-testable (form.test.ts), CM6-free and
// Svelte-free so the apply-body build + client validation are asserted without a DOM. The Svelte
// component (TemplatePicker.svelte) is a thin shell over this; the server stays the validation
// authority (templatestore.Substitute), this is the pre-flight that blocks the obvious faults locally.

import { ApiError } from '../api'
import { APPLY_ERROR_CODES, type ApplyErrorCode, type ParamSpec } from './types'

/**
 * The initial string value for each schema param's form field: a declared `default` rendered as text
 * (a JSON string default drops its quotes; a number/bool stringifies), else the first enum option, else
 * empty. Form fields are always strings — buildApplyParams coerces back on submit.
 */
export function formValueInit(specs: ParamSpec[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const s of specs) {
    if (s.default !== undefined && s.default !== null) {
      out[s.name] = typeof s.default === 'string' ? s.default : String(s.default)
    } else if (s.type === 'enum' && s.options && s.options.length > 0) {
      out[s.name] = s.options[0]
    } else {
      out[s.name] = ''
    }
  }
  return out
}

/**
 * The required params left empty — the client-side block (a required field must not reach /apply blank,
 * mirroring the server's missing_param 422). An optional param may stay empty; the server defaults it or
 * surfaces unresolved_placeholder if the source references it.
 */
export function missingRequired(specs: ParamSpec[], values: Record<string, string>): string[] {
  // String()-coerce defensively: a bound <input type="number"> can hand us a number at runtime.
  return specs.filter((s) => s.required && String(values[s.name] ?? '').trim() === '').map((s) => s.name)
}

/**
 * Build the /apply body `params` object from the form values. A `number` param whose value parses is
 * sent as a JSON number (the server's isNumeric accepts a numeric string too, but sending a real number
 * keeps the wire honest); everything else is sent as a string. An empty optional value is omitted (let
 * the server default it) — an empty required value never reaches here (missingRequired blocks first).
 */
export function buildApplyParams(specs: ParamSpec[], values: Record<string, string>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const s of specs) {
    const raw = String(values[s.name] ?? '').trim() // coerce: a number input may bind a number at runtime
    if (raw === '') continue // omit empties — required emptiness is blocked upstream
    if (s.type === 'number') {
      const n = Number(raw)
      out[s.name] = Number.isFinite(n) ? n : raw // a non-numeric string flows through to the server's invalid_param
    } else {
      out[s.name] = raw
    }
  }
  return out
}

/**
 * The server's machine error code for a failed /apply, for the readable-message lookup: the
 * `{success:false, code}` envelope's `code` (ApiError.details.code) when present, else the client-mapped
 * ApiError.code. Returns '' when nothing is extractable.
 */
export function applyErrorCode(err: ApiError): string {
  const serverCode = err.details?.['code']
  if (typeof serverCode === 'string' && serverCode !== '') return serverCode
  return err.code ?? ''
}

/** Whether a code is one of the known param-path 422 codes (drives the i18n message pick). */
export function isApplyErrorCode(code: string): code is ApplyErrorCode {
  return (APPLY_ERROR_CODES as readonly string[]).includes(code)
}
