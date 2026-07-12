<script lang="ts">
  // Firmware list + upload (W1 list, W2 upload — design 01-ota-spa §4.1/§4.2/§7):
  // read-only Resource<T> against GET /api/firmware, rendered through the shared
  // StateView (never an ambiguous blank, D19.13), plus the admin-gated multipart
  // upload (POST /api/firmware via apiUpload — NEVER apiFetch, which stamps
  // application/json and destroys the multipart boundary, §4.2).
  //
  // B6 (§5): `version` is a free TEXT field with NO charset CHECK — a Stored-XSS
  // vector at fleet scale. Every server-delivered string below renders as a text
  // node ({…}); NEVER {@html}. The AST gate (scripts/lint-no-html.ts) enforces
  // this structurally, and ota.test.ts pins it locally for this panel.
  import { onMount, onDestroy } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { apiFetch } from '../../lib/api'
  import { apiUpload } from '../../lib/api-binary'
  import { EventsClient } from '../../lib/events.svelte'
  import { conn } from '../../lib/conn.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import { useDirtyGuard } from '../../lib/dirtyGuard.svelte'
  import { sha256Hex } from '../../lib/webusb/flasher'
  import { FwGuardError, fwGuardText, guardedFirmwareUpload, otaErrorText } from '../../lib/ota/firmware'
  import type { FirmwareRegistered, FirmwareResponse } from '../../lib/ota/types'
  import { m } from '../../paraglide/messages.js'

  // `uploading` is bindable so OtaHome (the in-shell tab shell) can block a tab
  // switch away from Firmware while a blob is mid-flight — an in-shell tab swap
  // is a local {#if} render change, NOT an sv-router navigation, so useDirtyGuard
  // below (which hooks blockNavigation) does not by itself cover it (§4.2 Tab-
  // Wechsel-Kante; see OtaHome.svelte for the tab-disable wiring).
  let { uploading = $bindable(false) }: { uploading?: boolean } = $props()

  const firmware = new Resource<FirmwareResponse>(() => apiFetch<FirmwareResponse>('/api/firmware'))

  // A6 (E2): live `ota` reload-hint — another operator's firmware register re-fetches this list
  // without a manual reload. Created in onMount / closed in onDestroy: the in-shell tab swap
  // unmounts this panel (OtaHome {#if}), so the subscription lifecycle rides the mount exactly
  // like the LogViewer's stream does.
  let events = $state<EventsClient | null>(null)

  // mirror the live stream status into the shell-wide indicator (D19.14, LogViewer pattern)
  $effect(() => {
    conn.status = events?.status ?? 'idle'
  })

  const affordance = $derived(mutationAffordance(session.is_admin))

  let version = $state('')
  let fileInput = $state<HTMLInputElement | null>(null)
  let hasFile = $state(false)
  let phase = $state<'idle' | 'hashing' | 'uploading'>('idle')
  let progress = $state(0) // 0..1, real XHR upload-progress fraction (phase==='uploading' only)

  const canUpload = $derived(!uploading && !affordance.disabled && hasFile && version.trim() !== '')

  // Block a real navigation-away / tab-close while a firmware blob is mid-upload
  // (§4.2 Tab-Wechsel-Kante) — same guard PlaylistEditor registers for unsaved
  // edits (dirtyGuard.svelte.ts, Muster PlaylistEditor.svelte:106-108).
  $effect(() => {
    return useDirtyGuard(() => uploading, m['ota.fw.leave_confirm']())
  })

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

  function onFileChange(e: Event): void {
    const input = e.currentTarget as HTMLInputElement
    hasFile = (input.files?.length ?? 0) > 0
  }

  async function onSubmit(e: Event): Promise<void> {
    e.preventDefault()
    if (!session.is_admin || uploading) return // handler guard VOR dem Call, zusätzlich zur Affordance (§5 B1)
    const file = fileInput?.files?.[0]
    if (!file) return
    uploading = true
    phase = 'hashing'
    progress = 0
    try {
      const res = await guardedFirmwareUpload(file, version.trim(), {
        hash: async (bytes) => {
          const sha = await sha256Hex(bytes) // REUSE flasher.ts:42-44, kein neuer Hash-Helper
          phase = 'uploading'
          return sha
        },
        upload: (form, onProgress) => apiUpload<FirmwareRegistered>('/api/firmware', form, onProgress),
        onProgress: (frac) => {
          progress = frac
        },
      })
      notify.success(m['ota.fw.uploaded']({ version: res.version }))
      version = ''
      hasFile = false
      if (fileInput) fileInput.value = ''
      void firmware.reload()
    } catch (err) {
      if (err instanceof FwGuardError) notify.error(fwGuardText(err.reason))
      else notify.error(otaErrorText(err))
    } finally {
      uploading = false
      phase = 'idle'
      progress = 0
    }
  }

  onMount(() => {
    void firmware.load()
    events = new EventsClient({
      onOta: (hint) => {
        if (hint.kind === 'firmware') void firmware.reload()
      },
    })
    void events.connect()
  })

  onDestroy(() => {
    events?.close()
    conn.status = 'idle'
  })
</script>

<section class="firmware" aria-label={m['ota.fw.heading']()}>
  <h2>{m['ota.fw.heading']()}</h2>

  <form class="upload" onsubmit={onSubmit}>
    <label class="field">
      <span class="field-label">{m['ota.fw.version_label']()}</span>
      <input
        type="text"
        bind:value={version}
        maxlength="64"
        disabled={affordance.disabled || uploading}
        aria-label={m['ota.fw.version_label']()}
      />
    </label>
    <input
      bind:this={fileInput}
      type="file"
      onchange={onFileChange}
      disabled={affordance.disabled || uploading}
      aria-label={m['ota.fw.choose']()}
    />
    <button
      type="submit"
      disabled={!canUpload}
      title={affordance.disabled ? affordance.title : ''}
      aria-disabled={affordance['aria-disabled']}
    >
      {m['ota.fw.upload']()}
    </button>
    {#if phase === 'hashing'}
      <p class="hint" role="status">{m['ota.fw.sha_computing']()}</p>
    {:else if phase === 'uploading'}
      <div
        class="progress"
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(progress * 100)}
      >
        <div class="bar" style:width={`${Math.round(progress * 100)}%`}></div>
        <span class="pct">{m['ota.fw.uploading']({ pct: String(Math.round(progress * 100)) })}</span>
      </div>
    {/if}
  </form>

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
  .upload {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.6rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
  }
  .field-label {
    color: var(--fg-muted);
  }
  .field input {
    font: inherit;
  }
  .upload button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.35rem 0.9rem;
    cursor: pointer;
    font-size: 0.85rem;
  }
  .upload button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .upload button:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .hint {
    margin: 0;
    color: var(--fg-muted);
    font-size: 0.8rem;
  }
  .progress {
    position: relative;
    flex-basis: 100%;
    height: 1.25rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    overflow: hidden;
    background: var(--bg-muted, #eee);
  }
  .bar {
    height: 100%;
    background: var(--accent, #4a90d9);
    transition: width 0.1s linear;
  }
  .pct {
    position: absolute;
    inset: 0;
    display: grid;
    place-items: center;
    font-size: 0.75rem;
    color: var(--fg);
  }
</style>
