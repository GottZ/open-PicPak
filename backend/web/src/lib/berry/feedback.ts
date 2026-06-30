// Berry feedback model (Design 23 §4.5 / D23.4 / D23.9) — the cursor-advance ack, told honestly.
//
// THE load-bearing honesty seam: the device persists the served seq regardless of whether the script
// compiled or ran (cmd.c:371-372), and there is NO device→server result channel — the cursor crossing
// the enqueued seq is the ONLY signal. So "applied" here means DELIVERED + ATTEMPTED, never "succeeded".
// There is deliberately no success/✓ state in this module: a syntactically broken script lands in exactly
// the same "applied" bucket (T4). The only richer outcome is the device log deep-link (the on-device
// `script ok=%d` line, best-effort/async/unstructured — Doc 21).
//
// Initial state seeds from GET /api/berry/commands/recent (rehydration, D23.9 — SSE alone is a forward
// diff with no enqueue history); the c2cursor SSE then carries live applied_seq deltas. applied counts are
// monotonic (cursors only advance), so live data never regresses below the rehydration baseline.

const FLEET = '*'

export interface RecentCommand {
  seq: number
  serial: string
  note: string | null
  created_at: string
  operator_key_id: number | null
  target_count: number
  applied_count: number
}

export interface RecentResponse {
  success: true
  commands: RecentCommand[]
}

export interface C2CursorEvent {
  serial: string
  applied_seq: number
  last_poll_at: string | null
}

/** serial → latest applied_seq, accumulated from c2cursor SSE deltas. */
export type CursorMap = Record<string, number>

export type FeedbackState = 'applied' | 'partial' | 'pending'

export interface FeedbackRow {
  seq: number
  serial: string
  note: string | null
  created_at: string
  state: FeedbackState
  appliedCount: number
  targetCount: number
  /** the honest status line — "delivered + attempted", NEVER "succeeded" (D23.4). */
  label: string
  /** the device to deep-link the log view to (the only place the real outcome may surface); null for '*'. */
  logSerial: string | null
}

/** Validate + narrow a c2cursor SSE payload. Returns null on a malformed frame (never throws). */
export function parseCursorEvent(data: unknown): C2CursorEvent | null {
  if (typeof data !== 'object' || data === null) return null
  const d = data as Record<string, unknown>
  if (typeof d.serial !== 'string' || typeof d.applied_seq !== 'number') return null
  const lp = typeof d.last_poll_at === 'string' ? d.last_poll_at : null
  return { serial: d.serial, applied_seq: d.applied_seq, last_poll_at: lp }
}

/** Fold one cursor delta into the map (cursors only move forward — keep the max, never regress). */
export function applyCursorEvent(cursors: CursorMap, ev: C2CursorEvent): CursorMap {
  const prev = cursors[ev.serial] ?? -1
  if (ev.applied_seq <= prev) return cursors
  return { ...cursors, [ev.serial]: ev.applied_seq }
}

/** How many known device cursors have reached `seq` (the live '*' applied count). */
function appliedAcrossFleet(cursors: CursorMap, seq: number): number {
  let n = 0
  for (const s of Object.keys(cursors)) if (cursors[s] >= seq) n++
  return n
}

/**
 * Resolve one recent command to a feedback row, blending the rehydration baseline with live cursor data.
 * applied counts are monotonic: live data can only raise the count (cursors advance), capped at the target.
 */
export function resolveCommand(cmd: RecentCommand, cursors: CursorMap): FeedbackRow {
  const isFleet = cmd.serial === FLEET
  const targetCount = cmd.target_count

  let appliedCount: number
  if (isFleet) {
    appliedCount = Math.min(targetCount, Math.max(cmd.applied_count, appliedAcrossFleet(cursors, cmd.seq)))
  } else {
    const live = cmd.serial in cursors ? (cursors[cmd.serial] >= cmd.seq ? 1 : 0) : cmd.applied_count
    appliedCount = Math.max(cmd.applied_count, live)
  }

  const state: FeedbackState =
    appliedCount >= targetCount ? 'applied' : appliedCount > 0 ? 'partial' : 'pending'

  return {
    seq: cmd.seq,
    serial: cmd.serial,
    note: cmd.note,
    created_at: cmd.created_at,
    state,
    appliedCount,
    targetCount,
    label: labelFor(state, appliedCount, targetCount, isFleet),
    logSerial: isFleet ? null : cmd.serial,
  }
}

/** The honest label. "delivered + attempted" on every applied/partial row — NEVER a success claim (D23.4):
 *  the word is avoided entirely so neither the operator nor a future grep mistakes it for a result. */
function labelFor(state: FeedbackState, applied: number, target: number, isFleet: boolean): string {
  const tail = 'delivered + attempted (not proof it ran — check the device log)'
  if (state === 'applied') {
    return isFleet ? `${applied} of ${target} applied · ${tail}` : `applied · ${tail}`
  }
  if (state === 'partial') {
    return `${applied} of ${target} applied · ${tail} · rest pending`
  }
  // pending
  return isFleet
    ? `0 of ${target} applied — awaiting device polls (no ETA)`
    : 'pending — awaiting the device’s next poll (no ETA)'
}

/** Build the full feedback list (newest-first preserved from the rehydration order). */
export function buildFeedback(commands: RecentCommand[], cursors: CursorMap): FeedbackRow[] {
  return commands.map((c) => resolveCommand(c, cursors))
}
