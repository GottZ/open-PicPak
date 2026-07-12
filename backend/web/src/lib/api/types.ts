// Hand-maintained wire types (design 19 §2). No OpenAPI spec exists; the Go-side
// JSON golden tests are the drift anchor. One source comment per type.

// Source: cmd/admin/main.go whoami() + adminhttp.WriteOK — {success:true, kind,
// is_admin, scopes, label} (design 28 §4.3, Principal-based so a cookie session
// reports its real identity). The field is **is_admin** (snake_case) — NOT ctxd's
// `admin`; the read-only badge derives off this. Pinned by T9 golden-shape.
export interface WhoamiResponse {
  success: true
  kind: string
  is_admin: boolean
  scopes: string[]
  label: string
}

// Source: internal/devicestore/devicestore.go (Row) via GET /api/devices
// (cmd/admin/devices.go dh.list → {success:true, devices:[Row]}). bonded =
// device_auth.session_bootstrapped (COH1: a registered-but-never-polled device
// reads bonded=false); label is null until set; last_seen is null until first
// telemetry. The SSE roster snapshot/delta (design 19 §4.5) carries this same shape.
export interface Device {
  serial: string
  label: string | null
  channel: string
  last_seen: string | null
  bonded: boolean
}

// Source: cmd/admin/devices.go dh.list.
export interface DevicesResponse {
  success: true
  devices: Device[]
}

// Source: secrets.Meta via GET /api/secrets (secrets_api.go list → {secrets:[Meta]}, admin-only). The
// binding picker reads NAME only — never value/nonce/ciphertext/fingerprint (Doc 18 D18.3 / D25.7).
export interface SecretMeta {
  name: string
  key_version: number
  created_at: string
  rotated_at: string | null
}

export interface SecretsResponse {
  success: true
  secrets: SecretMeta[]
}

// Source: telemetryHandlers.config via GET /api/config (D22.13). Non-secret SPA config; empty strings
// when the env is unset (the editor hides the webhook URL then — no hostname baked into the SPA).
export interface ConfigResponse {
  success: true
  grafana_base_url: string
  webhook_base_url: string
}
