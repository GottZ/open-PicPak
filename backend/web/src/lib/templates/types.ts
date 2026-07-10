// Wire types for the template picker (A30 W7). Hand-maintained; the Go-side golden drift test
// (backend/web/template_types_golden_test.go) is the drift anchor — no OpenAPI spec (design 19 §2).
// Mirror the cmd/admin/template_http.go + template_apply_http.go JSON shapes and the templatestore
// row exactly. One source comment per type.

// Source: templatestore.Kind* (internal/templatestore/types.go) + the 0016 kind CHECK. The three
// disjoint template classes: a render_fn mints a faas_functions row, a berry_snippet enqueues onto the
// C2 queue, a playlist_preset is the Playlist axis's job. KINDS pins the vocabulary for the golden.
export type TemplateKind = 'render_fn' | 'berry_snippet' | 'playlist_preset'
export const KINDS: readonly TemplateKind[] = ['render_fn', 'berry_snippet', 'playlist_preset']

// Source: templatestore.checkParamType (internal/templatestore/substitute.go) — the param schema's
// type discriminator. 'url' is the K9 SSRF gate (host must sit in egress_allow); 'number'/'enum' are
// format checks; 'string' passes through. PARAM_TYPES pins the vocabulary for the golden.
export type ParamType = 'string' | 'number' | 'url' | 'enum'
export const PARAM_TYPES: readonly ParamType[] = ['string', 'number', 'url', 'enum']

// Source: templatestore.paramSpec (internal/templatestore/substitute.go) — one entry of a template's
// params JSONB schema. `default` rides as raw JSON so a number default stays a number; `options` backs
// the enum membership check.
export interface ParamSpec {
  name: string
  label: string
  type: ParamType
  required: boolean
  default?: unknown
  options?: string[]
}

// Source: templatestore.Summary via GET /api/templates (template_http.go list → {templates:[Summary]}).
// The list-view projection: no source, no params, no trust profile — the picker row.
export interface TemplateSummary {
  id: number
  name: string
  kind: TemplateKind
  // Multilingual "what does this template do" copy (locale→text). Rendered via localizedDescription
  // (lib/i18n.ts): locale→en→de→first→''. Carried on the list projection so the picker shows it without
  // a per-row detail fetch. May be {} (no description authored). Mirror of templatestore.Summary.Description.
  description: Record<string, string>
  builtin: boolean
  version: number
  updated_at: string
}

export interface TemplatesResponse {
  success: true
  templates: TemplateSummary[]
}

// Source: templatestore.Template via GET /api/templates/{id} (template_http.go get → {template}). The
// full row: source + params schema + the render_fn trust profile (egress_allow/secret_bindings/
// trigger_config carry NAMES/hosts only, never secret values, §5.4). builtin=true → the SPA badges it
// and treats "load" as "duplicate to an operator template" (the row itself is immutable, §5.3).
export interface TemplateDetail {
  id: number
  name: string
  kind: TemplateKind
  // Multilingual description map (locale→text); resolved via localizedDescription. Mirror of
  // templatestore.Template.Description. The FIELDS golden binds this set to the Go struct's json tags.
  description: Record<string, string>
  source: string
  params: ParamSpec[]
  egress_allow: string[]
  secret_bindings: string[]
  trigger_config?: Record<string, unknown>
  builtin: boolean
  version: number
  created_at: string
  updated_at: string
}

export interface TemplateResponse {
  success: true
  template: TemplateDetail
}

// Source: template_apply_http.go applyBerry → {kind, enqueued:[{serial,seq}]}. One enqueued row per
// target serial ("*" = the whole fleet).
export interface ApplyBerryResponse {
  success: true
  kind: 'berry_snippet'
  enqueued: { serial: string; seq: number }[]
}

// Source: template_apply_http.go applyRenderFn → {kind, id, name, serials}. ONE minted function
// (enabled=false), then n:1 binds over the explicit serials (empty when bind=false).
export interface ApplyRenderFnResponse {
  success: true
  kind: 'render_fn'
  id: number
  name: string
  serials: string[]
}

export type ApplyResponse = ApplyBerryResponse | ApplyRenderFnResponse

// Source: the 422 wire codes the /apply param path returns — templatestore.SubstituteError.Code
// (substitute.go: unknown_param / missing_param / unresolved_placeholder / invalid_param /
// egress_host_mismatch) plus berry_too_long (template_apply_http.go applyBerry, post-substitution octet
// cap). The picker maps each to a readable i18n string; the golden pins this set against the Go source.
export const APPLY_ERROR_CODES = [
  'unknown_param',
  'missing_param',
  'unresolved_placeholder',
  'invalid_param',
  'egress_host_mismatch',
  'berry_too_long',
] as const
export type ApplyErrorCode = (typeof APPLY_ERROR_CODES)[number]
