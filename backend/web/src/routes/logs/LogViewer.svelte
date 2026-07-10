<script lang="ts">
  import { onMount, onDestroy, tick } from 'svelte'
  import { apiFetch } from '../../lib/api'
  import { Resource } from '../../lib/resource.svelte'
  import { EventsClient } from '../../lib/events.svelte'
  import { conn } from '../../lib/conn.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import {
    type LogLine,
    type LogPage,
    type LogEvent,
    type LogSource,
    LOG_SOURCES,
    logEventToLines,
    reconcile,
    synthesizeView,
  } from './logs'
  import { m } from '../../paraglide/messages.js'

  // Log viewer (Design 21 §4.6). Read-only (D21.7): no mutation controls, so read-only operators see the
  // page unchanged. The device filter REUSES the shared DevicePicker (masterplan K13, D19.12) fed by
  // GET /api/devices — not a per-surface query — so a device is labelled identically everywhere. History
  // loads via the keyset REST query (§4.3); the SSE `log` stream appends live (W3); on reconnect a REST
  // backfill reconciles the gap (F2). Every log line + device label renders as a TEXT NODE — {@html} is
  // banned (D19.10: log lines are attacker-influenced).

  const MAX_LINES = 2000

  // shared device roster for the picker (K13)
  let devices = $state<Device[]>([])
  const roster = new Resource<DevicesResponse>(() => apiFetch<DevicesResponse>('/api/devices'))
  let selectedSerial = $state<string | null>(null)

  // source filter — both on by default (matches the server default)
  let sources = $state<Set<LogSource>>(new Set(LOG_SOURCES))

  // the line buffer + paging state
  let buffer = $state<LogLine[]>([])
  let oldestCursor = $state('')
  let reachedEdge = $state(false)
  let loadingOlder = $state(false)
  let follow = $state(true)
  const page = new Resource<LogPage>(() => apiFetch<LogPage>(`/api/logs?${params().toString()}`))

  let events = $state<EventsClient | null>(null)
  let viewport = $state<HTMLDivElement | null>(null)

  const view = $derived(synthesizeView(buffer))

  // mirror the live stream status into the shell-wide indicator (D19.14)
  $effect(() => {
    conn.status = events?.status ?? 'idle'
  })

  function params(before = ''): URLSearchParams {
    const p = new URLSearchParams()
    if (selectedSerial) p.set('serials', selectedSerial)
    p.set('sources', [...sources].join(','))
    if (before) p.set('before', before)
    return p
  }

  function matchesFilter(serial: string, source: string): boolean {
    if (selectedSerial && serial !== selectedSerial) return false
    return sources.has(source as LogSource)
  }

  async function scrollToBottom(): Promise<void> {
    await tick()
    if (viewport) viewport.scrollTop = viewport.scrollHeight
  }

  // (Re)load the newest page from scratch for the current filter. Resets the buffer so a filter change
  // never leaves stale lines from another device/source.
  async function reload(): Promise<void> {
    await page.load()
    if (page.data) {
      buffer = reconcile([], page.data.lines, MAX_LINES)
      oldestCursor = page.data.next_cursor
      reachedEdge = page.data.reached_edge
      if (follow) void scrollToBottom()
    }
  }

  // Scrollback: fetch one older page and PREPEND it (reconcile keeps order + dedups).
  async function loadOlder(): Promise<void> {
    if (loadingOlder || reachedEdge || !oldestCursor) return
    loadingOlder = true
    try {
      const older = await apiFetch<LogPage>(`/api/logs?${params(oldestCursor).toString()}`)
      buffer = reconcile(buffer, older.lines, MAX_LINES + older.lines.length)
      oldestCursor = older.next_cursor
      reachedEdge = older.reached_edge
    } catch {
      // a transient backfill failure is non-fatal; the next scroll retries
    } finally {
      loadingOlder = false
    }
  }

  function onLog(data: unknown): void {
    const ev = data as LogEvent
    if (!ev || !Array.isArray(ev.lines) || !matchesFilter(ev.serial, ev.source)) return
    buffer = reconcile(buffer, logEventToLines(ev), MAX_LINES)
    if (follow) void scrollToBottom()
  }

  // On reconnect (the stream returns to 'open' after a drop) backfill the gap: a newest-page REST fetch
  // reconciled into the buffer fills anything the SSE missed while disconnected — no loss, no dup (F2).
  let wasOpen = false
  $effect(() => {
    const open = events?.status === 'open'
    if (open && !wasOpen) void backfillOnReconnect()
    wasOpen = open
  })
  async function backfillOnReconnect(): Promise<void> {
    try {
      const fresh = await apiFetch<LogPage>(`/api/logs?${params().toString()}`)
      buffer = reconcile(buffer, fresh.lines, MAX_LINES)
      if (follow) void scrollToBottom()
    } catch {
      // best-effort; the live stream resumes regardless
    }
  }

  function onScroll(): void {
    if (!viewport) return
    // near the top → page older; leaving the bottom → drop follow (terminal convention)
    if (viewport.scrollTop < 48) void loadOlder()
    const atBottom = viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight < 8
    follow = atBottom
  }

  function toggleSource(s: LogSource): void {
    const next = new Set(sources)
    if (next.has(s)) {
      if (next.size > 1) next.delete(s) // never empty — at least one source stays on
    } else {
      next.add(s)
    }
    sources = next
  }

  // re-fetch whenever the filter changes (serial or sources)
  let lastFilter = ''
  $effect(() => {
    const key = `${selectedSerial ?? ''}|${[...sources].sort().join(',')}`
    if (key !== lastFilter) {
      lastFilter = key
      void reload()
    }
  })

  function dividerText(variant: 'gap' | 'suspect' | 'boot', bootCount: number | null): string {
    const boot = bootCount === null ? '' : m['logs.boot_suffix']({ n: bootCount })
    if (variant === 'gap') return m['logs.divider_gap']({ boot })
    if (variant === 'suspect') return m['logs.divider_suspect']({ boot })
    return m['logs.divider_boot']({ boot: bootCount ?? '?' })
  }

  function tsLabel(iso: string): string {
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleTimeString()
  }

  onMount(() => {
    void roster.load().then(() => {
      if (roster.data) devices = roster.data.devices
    })
    events = new EventsClient({
      onSnapshot: (s) => {
        devices = [...s.devices].sort((a, b) => a.serial.localeCompare(b.serial))
      },
      onLog,
    })
    void events.connect()
  })

  onDestroy(() => {
    events?.close()
    conn.status = 'idle'
  })

  const showEmpty = $derived(buffer.length === 0 && page.status === 'ready')
  const showLoading = $derived(buffer.length === 0 && (page.status === 'loading' || page.status === 'idle'))
  const showError = $derived(buffer.length === 0 && page.status === 'error')
</script>

<section class="logs">
  <header>
    <h1>{m['logs.title']()}</h1>
    <div class="follow">
      <label>
        <input type="checkbox" bind:checked={follow} onchange={() => follow && scrollToBottom()} />
        {m['logs.follow']()}
      </label>
      <button onclick={reload} disabled={page.status === 'loading'}>{m['logs.refresh']()}</button>
    </div>
  </header>

  <div class="filters">
    <div class="devicefilter">
      <DevicePicker {devices} bind:value={selectedSerial} placeholder={m['logs.filter_placeholder']()} />
      {#if selectedSerial}
        <button class="clear" onclick={() => (selectedSerial = null)}>{m['logs.show_all']()}</button>
      {/if}
    </div>
    <div class="sources">
      {#each LOG_SOURCES as s (s)}
        <label class="src">
          <input type="checkbox" checked={sources.has(s)} onchange={() => toggleSource(s)} />
          {s}
        </label>
      {/each}
    </div>
  </div>

  <div class="viewport" bind:this={viewport} onscroll={onScroll}>
    {#if showLoading}
      <p class="muted" aria-busy="true">{m['logs.loading']()}</p>
    {:else if showError}
      <div class="error" role="alert">
        <p>{page.error?.message}</p>
        {#if page.error?.requestId}<p class="muted">{m['app.request']({ id: page.error.requestId })}</p>{/if}
      </div>
    {:else if showEmpty}
      <p class="muted">{m['logs.empty']()}</p>
    {:else}
      {#if !reachedEdge}<p class="muted edge">{m['logs.scroll_older']()}</p>{/if}
      {#if reachedEdge}<p class="muted edge">{m['logs.start_history']()}</p>{/if}
      {#each view as item (item.key)}
        {#if item.kind === 'divider'}
          <div class="divider" class:suspect={item.variant === 'suspect'} role="separator">
            {dividerText(item.variant, item.bootCount)}
          </div>
        {:else}
          <div class="line">
            <span class="ts mono">{tsLabel(item.line.time)}</span>
            <span class="sn mono">{item.line.serial}</span>
            <span class="txt">{item.line.text}</span>
          </div>
        {/if}
      {/each}
    {/if}
  </div>
</section>

<style>
  .logs {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    height: calc(100vh - 7rem);
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
  .follow {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    font-size: 0.85rem;
  }
  .filters {
    display: flex;
    gap: 1.5rem;
    align-items: flex-start;
    flex-wrap: wrap;
  }
  .devicefilter {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .sources {
    display: flex;
    gap: 0.75rem;
    font-size: 0.85rem;
  }
  .src {
    display: flex;
    align-items: center;
    gap: 0.3rem;
  }
  .viewport {
    flex: 1;
    overflow-y: auto;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    font-family: monospace;
    font-size: 0.82rem;
    line-height: 1.5;
    background: var(--bg);
  }
  .line {
    display: flex;
    gap: 0.75rem;
    white-space: pre-wrap;
    word-break: break-word;
  }
  .ts {
    color: var(--fg-muted);
    flex-shrink: 0;
  }
  .sn {
    color: var(--accent);
    flex-shrink: 0;
  }
  .txt {
    flex: 1;
  }
  .divider {
    color: var(--warn);
    text-align: center;
    margin: 0.25rem 0;
    font-size: 0.78rem;
  }
  .divider.suspect {
    color: var(--danger);
  }
  .edge {
    text-align: center;
    font-size: 0.74rem;
  }
  .mono {
    font-family: monospace;
  }
  .muted {
    color: var(--fg-muted);
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
    font-size: 0.82rem;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .clear {
    align-self: flex-start;
  }
</style>
