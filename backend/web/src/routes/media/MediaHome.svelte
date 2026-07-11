<script lang="ts">
  // /media operator area shell (design 29 §1/§4.1): image upload + BWRY-accurate
  // preview + playlist editor. W1 scaffolds the navigable area with its (empty)
  // image library rendered through the shared StateView; the later waves (design
  // 29 §7 W4–W7) mount the uploader, the paged grid and the playlist editor onto
  // this same shell. The library fetcher resolves empty until A28's list route
  // lands (W5 swaps this single-shot Resource for the paged accumulator) — the
  // surface shows the StateView empty state, never an ambiguous blank (D19.13).
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import { Paged } from '../../lib/media/paged.svelte'
  import { VisibilityPool } from '../../lib/media/visibilitypool.svelte'
  import Uploader from '../../lib/media/Uploader.svelte'
  import PlaylistEditor from './PlaylistEditor.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import type { Image, ImagesResponse } from '../../lib/media/types'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import { m } from '../../paraglide/messages.js'

  // W5 (§6/§7): the paged accumulator the target-scale grid needs. Keyset over
  // GET /api/images (?after=<id>); the accumulator appends each page and dedups by
  // id, so paging never double-counts. Thumbnails load through the cacheable
  // same-origin /thumbnail route (§6 / E-A29-5) as lazy <img> — the grid never
  // pulls a multi-MB source per tile. The uploader refresh calls reload() (same
  // hook name as the W4 Resource), which resets to the first page.
  const library = new Paged<Image>(
    async (cursor) => {
      const res = await apiFetch<ImagesResponse>(
        cursor === null ? '/api/images' : `/api/images?after=${cursor}`,
      )
      return { items: res.images, next: res.next_cursor }
    },
    (image) => image.id,
  )

  // W6 (§6/§7): virtualisation. At target scale (500+ tiles) mounting every
  // thumbnail <img> holds 500 decoded bitmaps in RAM → the OOM the gate forbids.
  // One IntersectionObserver drives the whole grid: each tile's <li> registers via
  // the observe() action, and crossing the viewport (+200px overscan) mounts /
  // unmounts its <img> through the pool. Only the visibility window keeps a live
  // node; the pool's hard cap recycles the oldest so even an observer misfire can
  // never mount all 500.
  // W7 (§7): the /media area gains a second surface — the playlist editor — beside the image library.
  // A plain in-shell tab (no route split) keeps both under the one /media area the design defines, and
  // lets the editor's dirtyGuard treat a tab switch as an in-app navigation candidate.
  let activeTab = $state<'library' | 'playlists'>('library')

  const pool = new VisibilityPool()
  let observer: IntersectionObserver | null = null

  function tileId(node: Element): number {
    return Number((node as HTMLElement).dataset.imageId)
  }

  // Svelte action: register a tile <li> with the shared observer for its lifetime.
  // On unmount it both un-observes and exits the pool, so a deleted tile leaves no
  // stale live id behind.
  function observe(node: HTMLElement) {
    observer?.observe(node)
    return {
      destroy() {
        observer?.unobserve(node)
        const id = tileId(node)
        if (!Number.isNaN(id)) pool.exit(id)
      },
    }
  }

  // W6 delete: two-step armed confirm per tile (the shipped FunctionsEditor
  // pattern — confirmation is mandatory, no direct delete). A 409 image_in_use
  // (FK RESTRICT, §9(e)) keeps the row and surfaces the in-use reason; the backend
  // carries no numeric refcount, only the in-use signal. A 200 removes exactly the
  // one tile via the accumulator — no reload(), so the grid never flickers.
  let armed = $state<number | null>(null)
  let deleting = $state(false)
  const affordance = $derived(mutationAffordance(session.is_admin))

  async function doDelete(id: number): Promise<void> {
    if (!session.is_admin || deleting) return
    if (armed !== id) {
      armed = id // first click arms; a second click on the same tile commits.
      return
    }
    deleting = true
    try {
      await apiFetch(`/api/images/${id}`, { method: 'DELETE' })
      library.remove(id) // targeted list-invalidate, no full-reload flicker
      notify.success(m['media.library.delete_success']({ id }))
    } catch (e) {
      const err = toApiError(e)
      if (err.status === 409 || err.code === 'image_in_use') {
        notify.error(m['media.library.delete_in_use']({ id })) // row stays
      } else {
        notify.error(err)
      }
    } finally {
      deleting = false
      armed = null
    }
  }

  // W-A33.7 "Aufs Panel": put ONE image straight on a panel via the managed-playlist shortcut
  // (design/33 §4.5b). The button opens a picker popover for one image at a time; confirming sends
  // PUT /api/devices/{serial}/image {image_id} — an admin-only fleet mutation, so the button carries
  // the same mutationAffordance disable-with-reason discipline as delete (probe d, SPA half). The
  // confirm stays disabled until a target serial is chosen (no blind broadcast on a single-panel op).
  let deviceList = $state<Device[]>([])
  let panelFor = $state<number | null>(null) // image id being placed, or null (popover closed)
  let panelSerial = $state<string | null>(null)
  let sending = $state(false)
  const canSendPanel = $derived(panelFor !== null && panelSerial !== null && !affordance.disabled && !sending)

  function openPanel(id: number): void {
    if (!session.is_admin) return
    panelFor = id
    panelSerial = null
  }
  function closePanel(): void {
    panelFor = null
    panelSerial = null
  }

  async function submitPanel(): Promise<void> {
    if (panelFor === null || panelSerial === null || sending || !session.is_admin) return
    sending = true
    try {
      await apiFetch(`/api/devices/${panelSerial}/image`, {
        method: 'PUT',
        body: JSON.stringify({ image_id: panelFor }),
      })
      notify.success(m['media.panel.success']({ serial: panelSerial }))
      closePanel()
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      sending = false
    }
  }

  onMount(() => {
    // The picker feeds on the device list; a failure leaves it empty (the popover shows the picker's
    // own empty state — never a crash).
    void apiFetch<DevicesResponse>('/api/devices')
      .then((r) => (deviceList = r.devices))
      .catch(() => (deviceList = []))
    observer = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          const id = tileId(e.target)
          if (Number.isNaN(id)) continue
          if (e.isIntersecting) pool.enter(id)
          else pool.exit(id)
        }
      },
      { rootMargin: '200px' },
    )
    void library.reload()
    return () => observer?.disconnect()
  })
</script>

<section class="media">
  <header>
    <h1>{m['media.title']()}</h1>
    <p class="subtitle">{m['media.subtitle']()}</p>
    <div class="tabs" role="tablist" aria-label={m['media.title']()}>
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={activeTab === 'library'}
        aria-selected={activeTab === 'library'}
        onclick={() => (activeTab = 'library')}
      >
        {m['media.tab.library']()}
      </button>
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={activeTab === 'playlists'}
        aria-selected={activeTab === 'playlists'}
        onclick={() => (activeTab = 'playlists')}
      >
        {m['media.tab.playlists']()}
      </button>
    </div>
  </header>

  {#if activeTab === 'playlists'}
    <PlaylistEditor />
  {:else}
    <Uploader onUploaded={() => library.reload()} />

    <section class="library" aria-label={m['media.library.heading']()}>
      <h2>{m['media.library.heading']()}</h2>

      {#if panelFor !== null}
        <div
          class="panel-pop"
          role="dialog"
          aria-modal="true"
          aria-label={m['media.panel.title']({ id: panelFor })}
        >
          <h3>{m['media.panel.title']({ id: panelFor })}</h3>
          <DevicePicker devices={deviceList} bind:value={panelSerial} placeholder={m['media.panel.pick']()} />
          {#if panelSerial === null}
            <p class="pick-hint" role="status">{m['media.panel.device_required']()}</p>
          {/if}
          <div class="panel-actions">
            <button
              type="button"
              class="primary"
              disabled={!canSendPanel}
              title={affordance.disabled ? affordance.title : ''}
              aria-disabled={affordance['aria-disabled']}
              onclick={submitPanel}
            >
              {m['media.panel.confirm']()}
            </button>
            <button type="button" onclick={closePanel}>{m['media.panel.cancel']()}</button>
          </div>
        </div>
      {/if}
      <StateView resource={library} emptyText={m['media.library.empty']()}>
      {#snippet ready(images)}
        <ul class="grid">
          {#each images as image (image.id)}
            <li class="tile" data-image-id={image.id} use:observe>
              {#if pool.isLive(image.id)}
                <img
                  class="thumb"
                  src={`/api/images/${image.id}/thumbnail`}
                  alt={m['media.library.thumb_alt']({ id: image.id })}
                  loading="lazy"
                  width="160"
                  height="120"
                />
              {:else}
                <!-- Off-screen: a cheap aspect-ratio placeholder holds the tile's
                     scroll geometry without a decoded bitmap (the virtualisation). -->
                <div class="thumb placeholder" aria-hidden="true"></div>
              {/if}
              <span class="dims">{image.width}×{image.height}</span>
              {#if session.is_admin}
                <div class="tile-actions">
                  <button
                    type="button"
                    class="tile-panel"
                    disabled={affordance.disabled || sending}
                    title={affordance.disabled ? affordance.title : ''}
                    onclick={() => openPanel(image.id)}
                  >
                    {m['media.library.to_panel']()}
                  </button>
                  <button
                    type="button"
                    class="tile-delete"
                    class:armed={armed === image.id}
                    disabled={affordance.disabled || deleting}
                    title={affordance.disabled ? affordance.title : ''}
                    onclick={() => doDelete(image.id)}
                  >
                    {armed === image.id
                      ? m['media.library.delete_confirm']()
                      : m['media.library.delete']()}
                  </button>
                  {#if armed === image.id}
                    <button type="button" class="tile-cancel" onclick={() => (armed = null)}>
                      {m['media.library.delete_cancel']()}
                    </button>
                  {/if}
                </div>
              {/if}
            </li>
          {/each}
        </ul>
        {#if library.moreError}
          <div class="more-error" role="alert">
            <p>{library.moreError.message}</p>
            <button onclick={() => library.loadMore()}>{m['media.library.retry_more']()}</button>
          </div>
        {:else if library.hasMore}
          <button class="load-more" onclick={() => library.loadMore()} disabled={library.loadingMore}>
            {library.loadingMore ? m['media.library.loading_more']() : m['media.library.load_more']()}
          </button>
        {/if}
        {/snippet}
      </StateView>
    </section>
  {/if}
</section>

<style>
  .media {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
    max-width: 60rem;
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
  }
  .subtitle {
    margin: 0.35rem 0 0;
    color: var(--fg-muted);
    font-size: 0.875rem;
  }
  .tabs {
    display: flex;
    gap: 0.25rem;
    margin-top: 0.6rem;
  }
  .tab {
    background: transparent;
    border: 1px solid var(--border);
    border-bottom: none;
    border-radius: 6px 6px 0 0;
    color: var(--fg-muted);
    padding: 0.35rem 0.9rem;
    cursor: pointer;
    font-size: 0.875rem;
  }
  .tab:hover {
    color: var(--fg);
  }
  .tab.active {
    color: var(--fg);
    border-color: var(--accent);
    background: rgba(122, 162, 247, 0.1);
  }
  .library {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .grid {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
    gap: 0.75rem;
  }
  .tile {
    position: relative;
    border: 1px solid var(--border);
    border-radius: 6px;
    aspect-ratio: 4 / 3;
    overflow: hidden;
    background: var(--bg-muted, rgba(127, 127, 127, 0.08));
  }
  .thumb {
    width: 100%;
    height: 100%;
    object-fit: contain;
    display: block;
    /* The panel is a hard-pixel medium; keep the downscaled preview crisp. */
    image-rendering: pixelated;
  }
  .thumb.placeholder {
    /* No bitmap while off-screen — just the muted tile surface at the same size. */
    background: transparent;
  }
  .tile .dims {
    position: absolute;
    right: 0.25rem;
    bottom: 0.25rem;
    padding: 0.05rem 0.3rem;
    border-radius: 4px;
    background: rgba(0, 0, 0, 0.55);
    color: #fff;
    font-size: 0.7rem;
    line-height: 1.4;
  }
  .tile-actions {
    position: absolute;
    top: 0.25rem;
    right: 0.25rem;
    display: flex;
    gap: 0.25rem;
    opacity: 0;
    transition: opacity 0.12s;
  }
  .tile:hover .tile-actions,
  .tile:focus-within .tile-actions {
    opacity: 1;
  }
  .tile-delete,
  .tile-cancel,
  .tile-panel {
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.1rem 0.4rem;
    font-size: 0.7rem;
    cursor: pointer;
    background: rgba(0, 0, 0, 0.6);
    color: #fff;
  }
  .tile-delete:disabled,
  .tile-panel:disabled {
    opacity: 0.6;
    cursor: default;
  }
  .panel-pop {
    border: 1px solid var(--accent, #7aa2f7);
    border-radius: 8px;
    padding: 0.8rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    max-width: 30rem;
  }
  .panel-pop h3 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
  }
  .pick-hint {
    margin: 0;
    color: var(--fg-muted);
    font-size: 0.8rem;
  }
  .panel-actions {
    display: flex;
    gap: 0.5rem;
  }
  .panel-actions button {
    font-size: 0.85rem;
    padding: 0.35rem 0.7rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: transparent;
    color: var(--fg);
    cursor: pointer;
  }
  .panel-actions .primary {
    background: var(--accent, #7aa2f7);
    color: var(--accent-fg, #10131c);
    border-color: transparent;
    font-weight: 600;
  }
  .panel-actions .primary:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .tile-delete.armed {
    background: var(--danger);
    border-color: var(--danger);
  }
  .load-more {
    align-self: flex-start;
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.35rem 0.9rem;
    cursor: pointer;
  }
  .load-more:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .load-more:disabled {
    opacity: 0.6;
    cursor: default;
  }
  .more-error {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    color: var(--danger);
    font-size: 0.85rem;
  }
  .more-error p {
    margin: 0;
  }
  .more-error button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.2rem 0.7rem;
    cursor: pointer;
  }
</style>
