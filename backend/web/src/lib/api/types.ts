// Hand-maintained wire types (design 19 §2). No OpenAPI spec exists; the Go-side
// JSON golden tests are the drift anchor. One source comment per type.

// Source: cmd/admin/main.go whoami() + adminhttp.WriteOK — {success:true, key_id,
// is_admin, label}. The field is **is_admin** (snake_case) — NOT ctxd's `admin`
// (design 19 §2); the read-only badge derives off this. Pinned by T9 golden-shape.
export interface WhoamiResponse {
  success: true
  key_id: number
  is_admin: boolean
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
