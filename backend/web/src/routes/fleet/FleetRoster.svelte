<script lang="ts">
  import { onMount } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import type { Device, DevicesResponse } from '../../lib/api/types'

  // SCAFFOLD (design 19 §4.4): Doc 22 replaces this with the telemetry dashboard.
  // Its sole purpose is to prove the shell end-to-end — whoami → authed GET →
  // render. W4 wires the `devices` SSE delta onto this same roster (snapshot +
  // diff); for now it is a one-shot GET /api/devices.
  const roster = new Resource<DevicesResponse>(() => apiFetch<DevicesResponse>('/api/devices'))

  onMount(() => void roster.load())

  function seenLabel(d: Device): string {
    return d.last_seen ? new Date(d.last_seen).toLocaleString() : '—'
  }
</script>

<section class="fleet">
  <header>
    <h1>Fleet</h1>
    <button onclick={roster.reload} disabled={roster.status === 'loading'}>refresh</button>
  </header>

  {#if roster.status === 'loading' || roster.status === 'idle'}
    <p class="muted" aria-busy="true">loading roster…</p>
  {:else if roster.status === 'error'}
    <div class="error" role="alert">
      <p>{roster.error?.message}</p>
      {#if roster.error?.requestId}<p class="muted">request {roster.error.requestId}</p>{/if}
    </div>
  {:else if roster.data && roster.data.devices.length === 0}
    <p class="muted">No devices registered yet.</p>
  {:else if roster.data}
    <table>
      <thead>
        <tr><th>Serial</th><th>Label</th><th>Channel</th><th>Last seen</th><th>Bond</th></tr>
      </thead>
      <tbody>
        {#each roster.data.devices as d (d.serial)}
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
    justify-content: space-between;
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
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
