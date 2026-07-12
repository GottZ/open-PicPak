// Hand-maintained wire types (design 01-ota-spa §3). No OpenAPI spec exists; the
// Go-side JSON golden tests are the drift anchor (write.go / types.go / read.go
// under backend/cmd/backend, registerOTARoutes ota_http.go:38-49). One source
// comment per type.

// Source: write.go:134-140 (registerFirmware / listFirmware). Bewusst OHNE
// blob_path — der Server liefert ihn nie aus (§5 B6/Read-Tier, write.go:134).
export interface FirmwareRow {
  version: string
  sha256: string
  size_bytes: number
  created_at: string
}

// Source: write.go:161-164 (listChannels). default_version is null until a
// channel default has been set (PUT /api/channels/{name}).
export interface ChannelRow {
  name: string
  default_version: string | null
}

// Source: write.go:184-191 (listRollouts), sorted channel,serial (write.go:195).
// serial is '*' for a fleet-wide rollout (types.go:27).
export interface RolloutRow {
  id: number
  serial: string
  channel: string
  version: string
  state: 'active' | 'paused' | 'done'
  updated_at: string
}

// Source: internal/rollout/types.go:13-16 (resolve.go). `source` is the
// resolver-precedence tag (per-serial ▶ fleet ▶ channel-default, types.go:33).
// SourceChannelDefault carries the WIRE VALUE "channel-default" WITH A HYPHEN —
// the i18n key is `ota.resolve.source.channel_default` (underscore, §4.6). Map
// through an explicit Record<Source, MessageKey>, never string concatenation.
export type Source = 'serial' | 'fleet' | 'channel-default' | 'none'

export interface Resolved {
  version: string
  source: Source
}

// Response wrappers — every OTA read route envelopes WriteOK {success:true, …}
// (adminhttp.go:117-141).
export interface FirmwareResponse {
  success: true
  firmware: FirmwareRow[]
}

export interface ChannelsResponse {
  success: true
  channels: ChannelRow[]
}

export interface RolloutsResponse {
  success: true
  rollouts: RolloutRow[]
}

// Source: GET /api/resolve/{serial} (ota_http.go:257-266, read.go:18-24).
// source='none' is a fail-open 200, not a 404 (§4.4) — no target for the serial.
export interface ResolveResponse {
  success: true
  serial: string
  resolved: Resolved
}
