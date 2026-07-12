// Wire types for the FaaS editor (Design 25). Mirror the cmd/admin JSON shapes exactly — the Go-side
// golden tests are the drift anchor (no OpenAPI spec, design 19 §2). One source comment per type. The
// webhook token hash is NEVER on any read shape (K8): only create/rotate echo the plaintext once.

/** faas_functions.trigger_type (0009 CHECK) — the trigger discriminator. */
export type TriggerType = 'render' | 'schedule' | 'webhook'

// Source: faasstore.Summary via GET /api/functions (faas_http.go list → {functions:[Summary]}).
// The list-view projection: no source, no config, no token hash.
export interface FunctionSummary {
  id: number
  name: string
  trigger_type: TriggerType
  enabled: boolean
  version: number
}

export interface FunctionsResponse {
  success: true
  functions: FunctionSummary[]
}

// Source: fnFields() via GET /api/functions/{id} (faas_http.go get → {function, bound_serials}). The full
// function — source + config + the least-privilege secret/egress lists. trigger_config is the raw JSONB
// row (Policy=Data); it carries NO webhook token hash (K8).
export interface FunctionDetail {
  id: number
  name: string
  source: string
  version: number
  trigger_type: TriggerType
  trigger_config: Record<string, unknown>
  secret_bindings: string[]
  egress_allow: string[]
  enabled: boolean
  created_at: string
  updated_at: string
}

export interface FunctionResponse {
  success: true
  function: FunctionDetail
  bound_serials: string[]
}

// Source: faas_http.go create → {id, name, trigger_type, enabled, webhook_token?}. webhook_token is the
// plaintext, present ONLY for a webhook trigger and shown EXACTLY ONCE (D24.13) — never recoverable after.
export interface CreateFunctionResponse {
  success: true
  id: number
  name: string
  trigger_type: TriggerType
  enabled: boolean
  webhook_token?: string
}

// Source: faas_ui_http.go boundFunction via GET /api/devices/{serial}/render (forward read) — the function
// a device renders, or null. Never the source.
export interface BindingResponse {
  success: true
  binding: { function_id: number; name: string } | null
}

// Source: faas_ui_http.go boundDevices via GET /api/functions/{id}/devices (reverse read) — the blast
// radius (D25.9): every device a source edit re-renders on its next poll.
export interface BlastRadiusResponse {
  success: true
  serials: string[]
  count: number
}

