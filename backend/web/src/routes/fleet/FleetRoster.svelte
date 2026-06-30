<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import { EventsClient, type RosterDelta } from '../../lib/events.svelte'
  import type { Device, DevicesResponse } from '../../lib/api/types'

  // SCAFFOLD (design 19 §4.4): Doc 22 replaces this with the telemetry dashboard.
  // The shell's end-to-end liveness proof — whoami → authed GET → render → SSE
  // delta. `devices` is the single source: the initial GET paints it, then the
  // SSE snapshot (authoritative) + deltas keep it live (D19.7).
  let devices = $state<Device[]>([])
  const roster = new Resource<DevicesResponse>(() => apiFetch<DevicesResponse>('/api/devices'))

  let events = $state<EventsClient | null>(null)
  const live = $derived(events?.status ?? 'idle')

  function applyDelta(d: RosterDelta): void {
    if (d.op === 'remove') {
      devices = devices.filter((x) => x.serial !== d.device.serial)
      return
    }
    const i = devices.findIndex((x) => x.serial === d.device.serial)
    if (i >= 0) {
      devices[i] = d.device
    } else {
      devices = [...devices, d.device].sort((a, b) => a.serial.localeCompare(b.serial))
    }
  }

  onMount(() => {
    void roster.load().then(() => {
      // SSE snapshot becomes authoritative once it lands; the GET is the first paint.
      if (roster.data) devices = roster.data.devices
    })
    events = new EventsClient({
      onSnapshot: (s) => {
        devices = [...s.devices].sort((a, b) => a.serial.localeCompare(b.serial))
      },
      onDevices: applyDelta,
    })
    void events.connect()
  })

  onDestroy(() => events?.close())

  function seenLabel(d: Device): string {
    return d.last_seen ? new Date(d.last_seen).toLocaleString() : '—'
  }

  const showTable = $derived(devices.length > 0)
  const showLoading = $derived(!showTable && (roster.status === 'loading' || roster.status === 'idle'))
  const showError = $derived(!showTable && roster.status === 'error')
  const showEmpty = $derived(!showTable && roster.status === 'ready')
</script>

<section class="fleet">
  <header>
    <h1>Fleet</h1>
    <span class="live live-{live}" title="live event stream">{live}</span>
    <button onclick={roster.reload} disabled={roster.status === 'loading'}>refresh</button>
  </header>

  {#if showLoading}
    <p class="muted" aria-busy="true">loading roster…</p>
  {:else if showError}
    <div class="error" role="alert">
      <p>{roster.error?.message}</p>
      {#if roster.error?.requestId}<p class="muted">request {roster.error.requestId}</p>{/if}
    </div>
  {:else if showEmpty}
    <p class="muted">No devices registered yet.</p>
  {:else if showTable}
    <table>
      <thead>
        <tr><th>Serial</th><th>Label</th><th>Channel</th><th>Last seen</th><th>Bond</th></tr>
      </thead>
      <tbody>
        {#each devices as d (d.serial)}
          <tr>
            <td class="mono">{d.serial}</td>
            <td>{d.label ?? '—'}</td>
            <td class="mono">{d.channel}</td>
            <td class="muted">{seenLabel(d)}</td>
            <td>
              <span class="badge" class:bonded={d.bonded}>{d.bonded ? 'bonded' : 'pending'}</span>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</section>

<style>
  .fleet {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    max-width: 60rem;
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
  .live {
    font-family: monospace;
    font-size: 0.7rem;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--fg-muted);
  }
  .live-open {
    color: var(--ok);
  }
  .live-error,
  .live-connecting {
    color: var(--warn);
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
  .mono {
    font-family: monospace;
  }
  .muted {
    color: var(--fg-muted);
  }
  .badge {
    font-size: 0.72rem;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--fg-muted);
  }
  .badge.bonded {
    color: var(--ok);
    border-color: var(--ok);
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
