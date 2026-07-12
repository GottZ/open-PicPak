<script lang="ts">
  // Firmware list (W1, design 01-ota-spa §4.1/§7): read-only Resource<T> against
  // GET /api/firmware, rendered through the shared StateView (never an ambiguous
  // blank, D19.13). Upload lands in W2 — this wave is the list only.
  //
  // B6 (§5): `version` is a free TEXT field with NO charset CHECK — a Stored-XSS
  // vector at fleet scale. Every server-delivered string below renders as a text
  // node ({…}); NEVER {@html}. The AST gate (scripts/lint-no-html.ts) enforces
  // this structurally, and ota.test.ts pins it locally for this panel.
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { apiFetch } from '../../lib/api'
  import { Resource } from '../../lib/resource.svelte'
  import type { FirmwareResponse } from '../../lib/ota/types'
  import { m } from '../../paraglide/messages.js'

  const firmware = new Resource<FirmwareResponse>(() => apiFetch<FirmwareResponse>('/api/firmware'))

  /** Human-readable byte count (binary units) for size_bytes. */
  function humanSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`
    const units = ['KiB', 'MiB', 'GiB']
    let value = bytes / 1024
    let unit = 0
    while (value >= 1024 && unit < units.length - 1) {
      value /= 1024
      unit += 1
    }
    return `${value.toFixed(1)} ${units[unit]}`
  }

  /** Shortened sha256 for the table cell — the full value stays available via title=. */
  function shortSha(sha: string): string {
    return sha.length > 12 ? `${sha.slice(0, 12)}…` : sha
  }

  onMount(() => {
    void firmware.load()
  })
</script>

<section class="firmware" aria-label={m['ota.fw.heading']()}>
  <h2>{m['ota.fw.heading']()}</h2>

  <StateView resource={firmware} emptyText={m['ota.fw.empty']()} isEmpty={(data) => data.firmware.length === 0}>
    {#snippet ready(data)}
      <table>
        <thead>
          <tr>
            <th>{m['ota.fw.col.version']()}</th>
            <th>{m['ota.fw.col.sha']()}</th>
            <th>{m['ota.fw.col.size']()}</th>
            <th>{m['ota.fw.col.created']()}</th>
          </tr>
        </thead>
        <tbody>
          {#each data.firmware as row (row.version)}
            <tr>
              <td>{row.version}</td>
              <td title={row.sha256}>{shortSha(row.sha256)}</td>
              <td>{humanSize(row.size_bytes)}</td>
              <td>{row.created_at}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/snippet}
  </StateView>
</section>

<style>
  .firmware {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.875rem;
  }
  th,
  td {
    text-align: left;
    padding: 0.4rem 0.6rem;
    border-bottom: 1px solid var(--border);
  }
  th {
    color: var(--fg-muted);
    font-weight: 600;
    font-size: 0.8rem;
  }
</style>
