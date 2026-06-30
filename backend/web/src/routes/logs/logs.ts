// Log viewer logic (Design 21 §4.6, part b — the operator zone). The pure core of the /logs page: the
// wire types, the SSE row→line expansion, the gap/suspect/boot marker synthesis (F1), and the
// follow/scrollback buffer + reconnect-backfill reconcile (F2). No DOM/Svelte imports → node-testable.

// Source: internal/logquery.Line via GET /api/logs ({success, lines, next_cursor, reached_edge}). seq +
// idx are the per-line identity the client dedups + orders on (the durable server order outlives the
// ctid the keyset cursor uses internally).
export interface LogLine {
  time: string
  serial: string
  seq: number | null
  idx: number
  boot_count: number | null
  source: string
  gap: boolean
  suspect: boolean
  text: string
}

export interface LogPage {
  success: true
  lines: LogLine[]
  next_cursor: string
  reached_edge: boolean
}

export interface SerialsResponse {
  success: true
  serials: string[]
}

// Source: internal/logquery.TailEvent via the SSE `log` event — one ROW, its tokens as an array.
export interface LogEvent {
  serial: string
  time: string
  seq: number | null
  boot_count: number | null
  source: string
  gap: boolean
  suspect: boolean
  lines: string[]
}

/**
 * Expand one SSE row into per-token LogLines, mirroring the server's REST flatten exactly: gap/suspect
 * ride the FIRST token only (one divider per row boundary), boot_count + seq ride every token.
 */
export function logEventToLines(ev: LogEvent): LogLine[] {
  return ev.lines.map((text, idx) => ({
    time: ev.time,
    serial: ev.serial,
    seq: ev.seq,
    idx,
    boot_count: ev.boot_count,
    source: ev.source,
    gap: idx === 0 ? ev.gap : false,
    suspect: idx === 0 ? ev.suspect : false,
    text,
  }))
}

/**
 * A line's stable identity for dedup across the SSE stream and a REST backfill (F2). seq is the durable
 * per-device order; a null-seq ota-snapshot falls back to its time. (serial, order, idx) is unique.
 */
export function keyOf(l: LogLine): string {
  const ord = l.seq === null ? `t${l.time}` : `s${l.seq}`
  return `${l.serial}\x1f${ord}\x1f${l.idx}`
}

/** Display order: oldest→newest (terminal style), (time, serial, seq, idx) ascending. */
export function cmpLine(a: LogLine, b: LogLine): number {
  if (a.time !== b.time) return a.time < b.time ? -1 : 1
  if (a.serial !== b.serial) return a.serial < b.serial ? -1 : 1
  const sa = a.seq ?? 0
  const sb = b.seq ?? 0
  if (sa !== sb) return sa - sb
  return a.idx - b.idx
}

/**
 * Merge fetched lines (a REST backfill page or an SSE batch) into the live buffer, deduping by key and
 * keeping (time,serial,seq,idx) order — the F2 invariant: a reconnect backfill leaves NO gap and NO
 * duplicate. Caps to the newest `max` lines (bounded viewport; scrollback re-fetches older pages).
 */
export function reconcile(buffer: LogLine[], fetched: LogLine[], max: number): LogLine[] {
  const byKey = new Map<string, LogLine>()
  for (const l of buffer) byKey.set(keyOf(l), l)
  for (const l of fetched) byKey.set(keyOf(l), l) // identical content on a key collision — either wins
  const merged = [...byKey.values()].sort(cmpLine)
  return merged.length > max ? merged.slice(merged.length - max) : merged
}

// ---- marker synthesis (F1) ----

export type DividerVariant = 'gap' | 'suspect' | 'boot'

export type ViewItem =
  | { kind: 'line'; key: string; line: LogLine }
  | { kind: 'divider'; key: string; variant: DividerVariant; bootCount: number | null }

/**
 * Walk the ordered lines and inject Synthetic divider items distinct from device text (F1, D21.10): a
 * gap divider before a server gap=true line (REGARDLESS of any boot_count change — the property the
 * bc-only-derivation red misses), a boot divider on a boot_count transition the flag did not already
 * mark, and a suspect divider before a suspect=true line. Dividers carry only structured data; the
 * component formats their text client-side (i18n/CSS), so device text is never injected into a marker.
 */
export function synthesizeView(lines: LogLine[]): ViewItem[] {
  const out: ViewItem[] = []
  let prevBoot: number | null | undefined = undefined
  for (const line of lines) {
    const k = keyOf(line)
    if (line.gap) {
      out.push({ kind: 'divider', key: `gap:${k}`, variant: 'gap', bootCount: line.boot_count })
    } else if (prevBoot !== undefined && line.boot_count !== null && line.boot_count !== prevBoot) {
      out.push({ kind: 'divider', key: `boot:${k}`, variant: 'boot', bootCount: line.boot_count })
    }
    if (line.suspect) {
      out.push({ kind: 'divider', key: `suspect:${k}`, variant: 'suspect', bootCount: line.boot_count })
    }
    out.push({ kind: 'line', key: k, line })
    prevBoot = line.boot_count
  }
  return out
}

/** The source filter set the viewer offers; default shows both (matches the server default). */
export const LOG_SOURCES = ['telemetry', 'ota-snapshot'] as const
export type LogSource = (typeof LOG_SOURCES)[number]
