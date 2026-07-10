<script lang="ts">
  // The one device-picker (D19.12, K13): every "pick a device" surface (A21 log
  // filter, A23 Berry target, A25 bind UI) consumes THIS, fed by GET /api/devices
  // — label-primary / serial-secondary, recency sort, typeahead, and an inline
  // "last polled Nd ago — this will sit pending" warning on a stale target. Fixes
  // three findings at once: a silent device can't drop out of a filter, an
  // operator can't blind-enqueue to a long-offline device, and the same device is
  // labelled identically everywhere. Pure core in devicepicker.ts.
  import type { Device } from './api/types'
  import { filterDevices, sortByRecency, isStale, staleDays } from './devicepicker'
  import { m } from '../paraglide/messages.js'

  let {
    devices,
    value = $bindable(null),
    placeholder = m['devicepicker.placeholder'](),
  }: {
    devices: Device[]
    value?: string | null
    placeholder?: string
  } = $props()

  let query = $state('')
  const now = Date.now()

  const matches = $derived(sortByRecency(filterDevices(devices, query)))
  const selected = $derived(devices.find((d) => d.serial === value) ?? null)
  const selectedStale = $derived(selected !== null && isStale(selected, now))

  function label(d: Device): string {
    return d.label ?? d.serial
  }
  function staleNote(d: Device): string {
    const days = staleDays(d, now)
    if (days === null) return m['devicepicker.stale_never']()
    return m['devicepicker.stale_days']({ days })
  }
</script>

<div class="picker">
  <input type="search" bind:value={query} {placeholder} aria-label={m['devicepicker.aria']()} />
  <ul class="list" role="listbox">
    {#each matches as d (d.serial)}
      <li>
        <button
          type="button"
          role="option"
          class="opt"
          class:selected={d.serial === value}
          aria-selected={d.serial === value}
          onclick={() => (value = d.serial)}
        >
          <span class="label">{label(d)}</span>
          <span class="serial mono">{d.serial}</span>
          {#if d.bonded}<span class="badge">{m['devicepicker.bonded']()}</span>{/if}
          {#if isStale(d, now)}<span class="badge stale">{m['devicepicker.stale']()}</span>{/if}
        </button>
      </li>
    {:else}
      <li class="muted empty">{m['devicepicker.empty']()}</li>
    {/each}
  </ul>
  {#if selectedStale && selected}
    <p class="warn" role="status">⚠ {staleNote(selected)}</p>
  {/if}
</div>

<style>
  .picker {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    max-width: 28rem;
  }
  input {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.4rem 0.6rem;
    font-size: 0.9rem;
  }
  input:focus {
    outline: none;
    border-color: var(--accent);
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    max-height: 14rem;
    overflow-y: auto;
    border: 1px solid var(--border);
    border-radius: 6px;
  }
  .opt {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    border-bottom: 1px solid var(--border);
    color: var(--fg);
    padding: 0.4rem 0.6rem;
    cursor: pointer;
    font-size: 0.88rem;
  }
  .opt:hover {
    background: rgba(255, 255, 255, 0.04);
  }
  .opt.selected {
    background: rgba(122, 162, 247, 0.14);
  }
  .label {
    flex: 1;
  }
  .serial {
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .mono {
    font-family: monospace;
  }
  .badge {
    font-size: 0.66rem;
    padding: 0.02rem 0.4rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--ok);
  }
  .badge.stale {
    color: var(--warn);
    border-color: var(--warn);
  }
  .empty {
    padding: 0.5rem 0.6rem;
    font-size: 0.85rem;
  }
  .muted {
    color: var(--fg-muted);
  }
  .warn {
    margin: 0;
    color: var(--warn);
    font-size: 0.8rem;
  }
</style>
