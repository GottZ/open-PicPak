<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import { EventsClient, type RosterDelta } from '../../lib/events.svelte'
  import { conn } from '../../lib/conn.svelte'
  import DeviceCard from './DeviceCard.svelte'
  import {
    type FleetRow,
    type FleetResponse,
    type TelemetryEvent,
    applyTelemetry,
    applyMembership,
    healthGlyph,
    healthClass,
    noDataLabel,
    battLabel,
    channelDisplay,
    newestTime,
    ageLabel,
  } from './fleet'
  import { m } from '../../paraglide/messages.js'

  // Telemetry dashboard (Design 22 §4.4) — replaces the Doc 19 scaffold roster. The initial paint is one
  // enriched GET /api/fleet (running_ver/batt/health per device, silent devices as NO_DATA); the SSE
  // `telemetry` channel then carries per-row liveness/health deltas (D22.7) and the `devices` channel
  // membership. Read-only by nature — no mutation affordance, so the is_admin degradation is a non-event.
  // Every device/log-sourced string renders as a TEXT NODE ({@html} banned, D19.10 — labels/reset_reason
  // are attacker-influenceable). Three freshness clocks are never collapsed (D22.12): the poll clock, the
  // newest telemetry (data) clock, and the newest C2-contact (liveness) clock.

  let rows = $state<FleetRow[]>([])
  let serverTime = $state('')
  let selectedSerial = $state<string | null>(null)
  let nowMs = $state(Date.now())

  const fleet = new Resource<FleetResponse>(() => apiFetch<FleetResponse>('/api/fleet'))
  let events = $state<EventsClient | null>(null)

  // mirror the live stream status into the shell-wide indicator (D19.14)
  $effect(() => {
    conn.status = events?.status ?? 'idle'
  })

  async function reload(): Promise<void> {
    await fleet.load()
    if (fleet.data) {
      rows = fleet.data.fleet
      serverTime = fleet.data.server_time
    }
  }

  function onTelemetry(data: unknown): void {
    const ev = data as TelemetryEvent
    if (!ev || typeof ev.serial !== 'string') return
    rows = applyTelemetry(rows, ev)
  }

  function onDevices(d: RosterDelta): void {
    rows = applyMembership(rows, d.op, d.device)
  }

  // On reconnect (stream returns to 'open' after a drop) re-fetch the enriched fleet — the SSE deltas may
  // have missed pushes while disconnected; the REST snapshot is authoritative (mirrors the log viewer F2).
  let wasOpen = false
  $effect(() => {
    const open = events?.status === 'open'
    if (open && !wasOpen) void reload()
    wasOpen = open
  })

  onMount(() => {
    void reload()
    events = new EventsClient({ onTelemetry, onDevices })
    void events.connect()
    // tick the relative-age clock so "Nm ago" stays honest without a server round-trip
    const t = setInterval(() => (nowMs = Date.now()), 30_000)
    return () => clearInterval(t)
  })

  onDestroy(() => {
    events?.close()
    conn.status = 'idle'
  })

  const newestTelemetry = $derived(newestTime(rows.map((r) => (r.has_data ? r.time : null))))
  const newestC2 = $derived(newestTime(rows.map((r) => r.c2_last_seen)))

  const showLoading = $derived(rows.length === 0 && (fleet.status === 'loading' || fleet.status === 'idle'))
  const showError = $derived(rows.length === 0 && fleet.status === 'error')
  const showEmpty = $derived(rows.length === 0 && fleet.status === 'ready')

  function select(serial: string): void {
    selectedSerial = selectedSerial === serial ? null : serial
  }
</script>

<section class="fleet">
  <header>
    <h1>{m['fleet.title']()}</h1>
    <button onclick={reload} disabled={fleet.status === 'loading'}>{m['fleet.refresh']()}</button>
  </header>

  {#if showLoading}
    <p class="muted" aria-busy="true">{m['fleet.loading']()}</p>
  {:else if showError}
    <div class="error" role="alert">
      <p>{fleet.error?.message}</p>
      {#if fleet.error?.requestId}<p class="muted">{m['app.request']({ id: fleet.error.requestId })}</p>{/if}
    </div>
  {:else if showEmpty}
    <p class="muted">{m['fleet.empty']()}</p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>{m['fleet.col.health']()}</th><th>{m['fleet.col.serial']()}</th><th>{m['fleet.col.label']()}</th><th>{m['fleet.col.version']()}</th>
          <th>{m['fleet.col.battery']()}</th><th>{m['fleet.col.channel']()}</th><th>{m['fleet.col.last_seen']()}</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as r (r.serial)}
          {@const ch = channelDisplay(r)}
          <tr class:selected={r.serial === selectedSerial} onclick={() => select(r.serial)}>
            <td>
              <span class="glyph {healthClass(r.health)}" title={r.health}>{healthGlyph(r.health)}</span>
              <span class="hlabel {healthClass(r.health)}">{r.health}</span>
            </td>
            <td class="mono">{r.serial}</td>
            <td>{r.label ?? '—'}</td>
            <td class="mono">{r.has_data ? r.running_ver || '—' : noDataLabel(r)}</td>
            <td>{r.has_data ? battLabel(r.batt_pct, r.batt_mv) : '—'}</td>
            <td class="mono">
              {ch.text}{#if ch.mismatch}<span class="badge mismatch" title={m['fleet.mismatch_title']()}>≠</span>{/if}
            </td>
            <td class="muted">{ageLabel(r.reg_last_seen, nowMs)}</td>
          </tr>
        {/each}
      </tbody>
    </table>

    <footer class="clocks muted">
      <span>{m['fleet.clock_poll']({ age: ageLabel(serverTime, nowMs) })}</span>
      <span>{m['fleet.clock_report']({ age: ageLabel(newestTelemetry, nowMs) })}</span>
      <span>{m['fleet.clock_c2']({ age: ageLabel(newestC2, nowMs) })}</span>
    </footer>
  {/if}

  {#if selectedSerial}
    <DeviceCard serial={selectedSerial} onclose={() => (selectedSerial = null)} />
  {/if}
</section>

<style>
  .fleet {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    max-width: 70rem;
  }
  header {
    display: flex;
    align-items: baseline;
    gap: 0.75rem;
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    flex: 1;
  }
  table {
    border-collapse: collapse;
    width: 100%;
    font-size: 0.9rem;
  }
  th,
  td {
    text-align: left;
    padding: 0.4rem 0.75rem;
    border-bottom: 1px solid var(--border);
  }
  th {
    font-size: 0.72rem;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--fg-muted);
    font-weight: 600;
  }
  tbody tr {
    cursor: pointer;
  }
  tbody tr:hover {
    background: rgba(255, 255, 255, 0.04);
  }
  tbody tr.selected {
    background: rgba(122, 162, 247, 0.14);
  }
  .glyph {
    font-size: 1rem;
    margin-right: 0.4rem;
  }
  .hlabel {
    font-size: 0.7rem;
    letter-spacing: 0.04em;
  }
  .ok {
    color: var(--ok);
  }
  .warn {
    color: var(--warn);
  }
  .danger {
    color: var(--danger);
  }
  .muted {
    color: var(--fg-muted);
  }
  .mono {
    font-family: monospace;
  }
  .badge.mismatch {
    color: var(--warn);
    border: 1px solid var(--warn);
    border-radius: 999px;
    padding: 0 0.35rem;
    margin-left: 0.35rem;
    font-size: 0.72rem;
  }
  .clocks {
    display: flex;
    gap: 1.5rem;
    font-size: 0.76rem;
    flex-wrap: wrap;
  }
  .error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.2rem 0.7rem;
    cursor: pointer;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
</style>
