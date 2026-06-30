// Fleet dashboard logic (Design 22 §4.4 — the operator zone). The pure core of the /fleet page: the wire
// types, the SSE telemetry-delta merge (F1), the health → glyph/class mapping (F2), the sentinel n/a
// rendering (F3), the assigned-vs-reported channel display (F4), plus the freshness clocks, age labels,
// sparkline path and Grafana deep-link. No DOM/Svelte imports → node-testable (mirrors logs.ts).

// Source: cmd/admin/telemetry_http.go fleetView = embedded internal/telemetry.FleetRow + the
// server-computed health/reasons/channel_mismatch. A Go Null[T] marshals to value-or-null, so every
// metric is `number | null` here. has_data=false → the lateral telemetry join missed (NO_DATA), the
// metric fields are null and time is the zero value.
export type Health = 'OK' | 'LOW_BATT' | 'BAD_BOOTS' | 'BROWNOUT' | 'OFFLINE_STALE' | 'NO_DATA'

export interface FleetRow {
  serial: string
  label: string | null
  assigned_channel: string
  reg_last_seen: string | null
  bonded: boolean
  c2_last_seen: string | null
  has_data: boolean
  time: string
  batt_mv: number | null
  batt_pct: number | null
  bad_boots: number | null
  boot_count: number | null
  reset_reason: string | null
  usb: boolean | null
  uptime_ms: number | null
  running_ver: string
  reported_channel: string
  diag_ota_rr: number | null
  // added by fleetView:
  health: Health
  reasons: string[]
  channel_mismatch: boolean
}

export interface FleetResponse {
  success: true
  fleet: FleetRow[]
  server_time: string
}

// Source: internal/telemetry.Event via the SSE `telemetry` event — one per-device latest-telemetry delta.
export interface TelemetryEvent {
  serial: string
  time: string
  batt_pct: number | null
  batt_mv: number | null
  running_ver: string
  health: Health
  last_seen: string | null
  channel_mismatch: boolean
}

// Source: internal/telemetry.Extra (opportunistic JSONB metrics; n/a when absent).
export interface TelemetryExtra {
  rssi: number | null
  heap: number | null
  temp: number | null
  tx: number | null
  present: boolean
}

// Source: internal/telemetry.DeviceTelemetry via GET /api/devices/{serial}/telemetry (latest).
export interface DeviceTelemetry {
  serial: string
  time: string
  batt_mv: number | null
  batt_pct: number | null
  bad_boots: number | null
  boot_count: number | null
  reset_reason: string | null
  usb: boolean | null
  uptime_ms: number | null
  running_ver: string
  device_channel: string
  diag_run_part: string | null
  diag_runstate: number | null
  diag_o0: number | null
  diag_o1: number | null
  diag_inv: string | null
  diag_boots: number | null
  diag_rr: number | null
  diag_ota_rr: number | null
  diag_mv: number | null
  diag_mv_err: number | null
  diag_stage: string | null
  extra: TelemetryExtra
}

// Source: internal/telemetry.HistoryPoint (the sparkline/history window).
export interface HistoryPoint {
  time: string
  batt_mv: number | null
  batt_pct: number | null
  bad_boots: number | null
  boot_count: number | null
  reset_reason: string | null
  uptime_ms: number | null
  diag_ota_rr: number | null
}

// Source: cmd/admin/telemetry_http.go deviceTelemetry — the enriched header row + full detail + history.
export interface DeviceTelemetryResponse {
  success: true
  fleet: FleetRow
  latest: DeviceTelemetry | null
  history: HistoryPoint[]
  rollback_brownout: boolean
  server_time: string
}

// Source: cmd/admin/telemetry_http.go config (D22.13). Empty strings → the SPA hides the deep-links.
export interface AppConfig {
  success: true
  grafana_base_url: string
  webhook_base_url: string
}

// A bare roster device (the SSE `devices`/`snapshot` payload — membership, not telemetry).
export interface RosterDevice {
  serial: string
  label: string | null
  channel: string
  last_seen: string | null
  bonded: boolean
}

// ---- F1: merge an SSE telemetry delta into the fleet rows ----

/**
 * Apply one telemetry SSE delta: update the MATCHING serial's live fields (battery/version/health/
 * last_seen) and leave every other row untouched. Returns a NEW array (Svelte reactivity). A delta for a
 * serial not in the roster is ignored — membership rides the `devices` channel (applyMembership), never
 * this one (the red: mutating in place, or replacing the whole roster, drops other rows' live state).
 */
export function applyTelemetry(rows: FleetRow[], ev: TelemetryEvent): FleetRow[] {
  const i = rows.findIndex((r) => r.serial === ev.serial)
  if (i < 0) return rows
  const next = rows.slice()
  next[i] = {
    ...rows[i],
    has_data: true,
    time: ev.time,
    batt_pct: ev.batt_pct,
    batt_mv: ev.batt_mv,
    running_ver: ev.running_ver,
    health: ev.health,
    reg_last_seen: ev.last_seen ?? rows[i].reg_last_seen,
    channel_mismatch: ev.channel_mismatch,
  }
  return next
}

// ---- F2: roster membership add/remove (the `devices` channel) ----

/** A registered-but-silent placeholder row for a newly-added device, until its first telemetry push. */
export function noDataRow(d: RosterDevice): FleetRow {
  return {
    serial: d.serial,
    label: d.label,
    assigned_channel: d.channel,
    reg_last_seen: d.last_seen,
    bonded: d.bonded,
    c2_last_seen: null,
    has_data: false,
    time: '',
    batt_mv: null,
    batt_pct: null,
    bad_boots: null,
    boot_count: null,
    reset_reason: null,
    usb: null,
    uptime_ms: null,
    running_ver: '',
    reported_channel: '',
    diag_ota_rr: null,
    health: 'NO_DATA',
    reasons: ['silent'],
    channel_mismatch: false,
  }
}

/**
 * Apply a roster membership delta: a removed serial drops; a new serial is inserted as a NO_DATA
 * placeholder (telemetry enriches it later); an existing serial's identity columns (label/channel/bonded/
 * last_seen) update WITHOUT clobbering its telemetry-derived fields. Returns a new, serial-sorted array.
 */
export function applyMembership(rows: FleetRow[], op: 'upsert' | 'remove', d: RosterDevice): FleetRow[] {
  if (op === 'remove') return rows.filter((r) => r.serial !== d.serial)
  const i = rows.findIndex((r) => r.serial === d.serial)
  if (i >= 0) {
    const next = rows.slice()
    next[i] = { ...rows[i], label: d.label, assigned_channel: d.channel, bonded: d.bonded, reg_last_seen: d.last_seen }
    return next
  }
  return [...rows, noDataRow(d)].sort((a, b) => a.serial.localeCompare(b.serial))
}

// ---- F2: health glyph + class ----

export function healthGlyph(h: Health): string {
  switch (h) {
    case 'OK':
      return '●'
    case 'LOW_BATT':
      return '▽'
    case 'BAD_BOOTS':
      return '↻'
    case 'BROWNOUT':
      return '⚡'
    case 'OFFLINE_STALE':
      return '○'
    case 'NO_DATA':
      return '–'
  }
}

export function healthClass(h: Health): 'ok' | 'warn' | 'danger' | 'muted' {
  switch (h) {
    case 'OK':
      return 'ok'
    case 'LOW_BATT':
    case 'BAD_BOOTS':
      return 'warn'
    case 'BROWNOUT':
    case 'OFFLINE_STALE':
      return 'danger'
    case 'NO_DATA':
      return 'muted'
  }
}

/** The NO_DATA split (D22.12): C2-alive (telemetry unwired, the current normal) vs truly silent. */
export function noDataLabel(row: FleetRow): string {
  if (row.has_data) return ''
  return row.reasons.includes('c2_alive') ? 'C2-alive, awaiting telemetry' : 'silent — no C2 contact'
}

// ---- F3: sentinel n/a rendering ----

/** Render a nullable metric: a SQL NULL or a per-column sentinel (mapped to null server-side) → 'n/a'. */
export function metricLabel(v: number | null, unit = ''): string {
  return v === null ? 'n/a' : `${v}${unit}`
}

/** Battery as "p% (mV)" with each field independently sentinel-aware. */
export function battLabel(pct: number | null, mv: number | null): string {
  if (pct === null && mv === null) return 'n/a'
  const p = pct === null ? 'n/a' : `${pct}%`
  const m = mv === null ? '' : ` (${mv}mV)`
  return `${p}${m}`
}

// ---- F4: assigned vs reported channel ----

/**
 * The channel display: the AUTHORITATIVE assigned channel, plus "(reports <x>)" ONLY when the server
 * flagged a mismatch (channel_mismatch — gated by ADMIN_TELEMETRY_CHANNEL_MISMATCH_WARN). The red: showing
 * the device-reported channel as authoritative would mislead a rollout read (Doc 20).
 */
export function channelDisplay(row: FleetRow): { text: string; mismatch: boolean } {
  if (row.channel_mismatch && row.reported_channel) {
    return { text: `${row.assigned_channel} (reports ${row.reported_channel})`, mismatch: true }
  }
  return { text: row.assigned_channel, mismatch: false }
}

// ---- freshness clocks + age ----

/** The newest of a set of ISO timestamps (skipping null/invalid), or null when none. */
export function newestTime(values: (string | null)[]): string | null {
  let bestMs: number | null = null
  let bestIso: string | null = null
  for (const v of values) {
    if (!v) continue
    const t = new Date(v).getTime()
    if (!Number.isNaN(t) && (bestMs === null || t > bestMs)) {
      bestMs = t
      bestIso = v
    }
  }
  return bestIso
}

/** A coarse "Ns/m/h/d ago" age, or '—' for null/invalid. */
export function ageLabel(iso: string | null, nowMs: number): string {
  if (!iso) return '—'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return '—'
  const s = Math.max(0, Math.floor((nowMs - t) / 1000))
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

// ---- sparkline (inline SVG path) ----

/**
 * Build an SVG path for a value series (left→right = oldest→newest; caller reverses the newest-first
 * history). null points (n/a) are skipped, never plotted as zero. Fewer than two real points → '' (the
 * component renders 'n/a' instead of a misleading flat line).
 */
export function sparklinePath(values: (number | null)[], width: number, height: number): string {
  const pts: { v: number; i: number }[] = []
  values.forEach((v, i) => {
    if (v !== null) pts.push({ v, i })
  })
  if (pts.length < 2) return ''
  const lastIdx = values.length - 1 || 1
  const min = Math.min(...pts.map((p) => p.v))
  const max = Math.max(...pts.map((p) => p.v))
  const span = max - min || 1
  return pts
    .map((p, k) => {
      const x = (p.i / lastIdx) * width
      const y = height - ((p.v - min) / span) * height
      return `${k === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
}

// ---- Grafana deep-link (D22.13 — hidden when the base is unset) ----

/** The per-device Grafana deep-link, or '' when the base URL is unset (the SPA then hides the link). */
export function grafanaLink(base: string, serial: string): string {
  if (!base) return ''
  const sep = base.includes('?') ? '&' : '?'
  return `${base}${sep}var-serial=${encodeURIComponent(serial)}`
}

// ---- /api/config, fetched once and cached (D22.13 — "once at bootstrap") ----

let configPromise: Promise<AppConfig> | null = null

/** Load /api/config once and cache it across device cards (the injected fetcher keeps this node-testable). */
export function loadAppConfig(fetcher: () => Promise<AppConfig>): Promise<AppConfig> {
  if (!configPromise) configPromise = fetcher()
  return configPromise
}

/** Test-only: drop the cached config promise. */
export function resetAppConfigCache(): void {
  configPromise = null
}
