<script lang="ts">
  // Playlist editor (design 29 §7 W7) — the second surface in the /media area (mounted as a tab beside
  // the W1–W6 image library in MediaHome). It lists + creates playlists, edits name/order_mode/interval_s,
  // manages items (add from the library, remove, drag-reorder), binds a device, and is the FIRST consumer
  // of the dirty-state guard (D19.15): an unsaved edit + an in-app navigation routes through a confirm.
  //
  // Reorder is buffered as a draft (the pure reducer in lib/media/playlist.ts) and persisted on Save via
  // the full-order PATCH /items (the server rejects a partial order 422). Item add/remove are immediate
  // single-item calls. All item pages are loaded up front because the reorder body must be the COMPLETE
  // item set — a partial page could not produce a valid item_ids order.
  import { onDestroy } from 'svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import StateView from '../../lib/StateView.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import { Paged } from '../../lib/media/paged.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import { useDirtyGuard } from '../../lib/dirtyGuard.svelte'
  import { loadDraft, saveDraft, clearDraft } from '../../lib/draft'
  import {
    reorder,
    itemIds,
    draftOf,
    playlistDirty,
    serializeDraft,
    parseDraft,
    type PlaylistDraft,
  } from '../../lib/media/playlist'
  import { ORDER_MODES } from '../../lib/media/types'
  import type {
    Image,
    ImagesResponse,
    OrderMode,
    Playlist,
    PlaylistDetailResponse,
    PlaylistItem,
    PlaylistSummary,
    PlaylistsResponse,
  } from '../../lib/media/types'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import { m } from '../../paraglide/messages.js'

  const affordance = $derived(mutationAffordance(session.is_admin))

  // ---- playlist list (left column) + the library used by the add-picker ----
  const list = new Paged<PlaylistSummary>(
    async (cursor) => {
      const res = await apiFetch<PlaylistsResponse>(
        cursor === null ? '/api/playlists' : `/api/playlists?after=${cursor}`,
      )
      return { items: res.playlists, next: res.next_cursor }
    },
    (p) => p.id,
  )
  const library = new Paged<Image>(
    async (cursor) => {
      const res = await apiFetch<ImagesResponse>(cursor === null ? '/api/images' : `/api/images?after=${cursor}`)
      return { items: res.images, next: res.next_cursor }
    },
    (img) => img.id,
  )
  void list.reload()
  void library.reload()

  // device roster for the bind picker (best-effort; GET /api/devices is auth-gated).
  let deviceList = $state<Device[]>([])
  void apiFetch<DevicesResponse>('/api/devices')
    .then((r) => (deviceList = r.devices))
    .catch(() => {})

  // ---- selection + loaded detail ----
  let selectedId = $state<number | null>(null)
  let detail = $state<Playlist | null>(null)
  let items = $state<PlaylistItem[]>([])
  let serverItems = $state<PlaylistItem[]>([]) // server order — the baseline the draft diffs against
  let detailStatus = $state<'idle' | 'loading' | 'error'>('idle')
  let detailError = $state<string | null>(null)

  // ---- editable meta ----
  let nameEdit = $state('')
  let orderModeEdit = $state<OrderMode>('sequential')
  let intervalEdit = $state(900)
  let baseline = $state<PlaylistDraft | null>(null)

  // ---- create form ----
  let newName = $state('')
  let newOrderMode = $state<OrderMode>('sequential')
  let newInterval = $state(900)
  let creating = $state(false)

  let saving = $state(false)
  let mutating = $state(false)
  let deleteArmed = $state(false)
  let bindTarget = $state<string | null>(null)
  let dragFrom = $state<number | null>(null)

  const currentDraft = $derived<PlaylistDraft | null>(
    detail === null ? null : draftOf(nameEdit, orderModeEdit, intervalEdit, items),
  )
  const dirty = $derived(baseline !== null && currentDraft !== null && playlistDirty(baseline, currentDraft))
  const createValid = $derived(newName.trim() !== '' && newInterval > 0)

  // dirty-state guard (D19.15) — registered once; the getter reads the live `dirty` at navigation time.
  $effect(() => {
    return useDirtyGuard(() => dirty, m['media.playlist.unsaved_confirm']())
  })

  // Draft persistence: mirror the FaaS editor — persist while dirty, clear when clean/saved.
  $effect(() => {
    if (selectedId === null || baseline === null || currentDraft === null) return
    if (playlistDirty(baseline, currentDraft)) saveDraft(`playlist:${selectedId}`, serializeDraft(currentDraft))
    else clearDraft(`playlist:${selectedId}`)
  })

  // reset the armed delete + bind target whenever the selection changes.
  $effect(() => {
    void selectedId
    deleteArmed = false
    bindTarget = null
  })

  onDestroy(() => {}) // guard cleanup is returned by the $effect above

  // Reorder the server items to a persisted draft's id order, dropping unknown ids and appending any
  // server item the draft missed (defensive — a concurrently-added item must not vanish), then renumber.
  function applyOrder(all: PlaylistItem[], ids: number[]): PlaylistItem[] {
    const byId = new Map(all.map((it) => [it.id, it]))
    const ordered: PlaylistItem[] = []
    for (const id of ids) {
      const it = byId.get(id)
      if (it) {
        ordered.push(it)
        byId.delete(id)
      }
    }
    for (const it of all) if (byId.has(it.id)) ordered.push(it)
    return ordered.map((it, i) => (it.position === i ? it : { ...it, position: i }))
  }

  async function loadDetail(id: number): Promise<void> {
    selectedId = id
    detailStatus = 'loading'
    detailError = null
    try {
      // Load the playlist and EVERY item page — the reorder body needs the complete set.
      const first = await apiFetch<PlaylistDetailResponse>(`/api/playlists/${id}`)
      const all: PlaylistItem[] = [...first.items]
      let next = first.next_cursor
      while (next !== null) {
        const page = await apiFetch<PlaylistDetailResponse>(`/api/playlists/${id}?after=${next}`)
        all.push(...page.items)
        next = page.next_cursor
      }
      const pl = first.playlist
      detail = pl
      serverItems = all
      baseline = draftOf(pl.name, pl.order_mode, pl.interval_s, all) // baseline = server truth
      const draft = parseDraft(loadDraft(`playlist:${id}`))
      if (draft) {
        nameEdit = draft.name
        orderModeEdit = draft.order_mode
        intervalEdit = draft.interval_s
        items = applyOrder(all, draft.item_ids)
      } else {
        nameEdit = pl.name
        orderModeEdit = pl.order_mode
        intervalEdit = pl.interval_s
        items = all
      }
      detailStatus = 'idle'
    } catch (e) {
      detail = null
      detailError = toApiError(e).message
      detailStatus = 'error'
    }
  }

  // Map a server error to a localised message via its machine `code` (WriteErr envelope), else the raw text.
  function errText(e: unknown): string {
    const err = toApiError(e)
    const code = (err.details?.['code'] as string | undefined) ?? err.code
    switch (code) {
      case 'name_conflict':
        return m['media.playlist.error.name_conflict']()
      case 'invalid_name':
        return m['media.playlist.error.invalid_name']()
      case 'invalid_policy':
        return m['media.playlist.error.invalid_interval']()
      case 'reorder_incomplete':
        return m['media.playlist.error.reorder_incomplete']()
      case 'unknown_image':
        return m['media.playlist.error.unknown_image']()
      case 'playlist_in_use':
        return m['media.playlist.delete_in_use']()
      default:
        return err.message
    }
  }

  async function doCreate(): Promise<void> {
    if (!createValid || creating || !session.is_admin) return
    creating = true
    try {
      const res = await apiFetch<{ success: boolean; playlist: Playlist }>('/api/playlists', {
        method: 'POST',
        body: JSON.stringify({ name: newName.trim(), order_mode: newOrderMode, interval_s: newInterval }),
      })
      newName = ''
      newOrderMode = 'sequential'
      newInterval = 900
      await list.reload()
      await loadDetail(res.playlist.id)
    } catch (e) {
      notify.error(errText(e))
    } finally {
      creating = false
    }
  }

  async function doSave(): Promise<void> {
    if (!detail || !dirty || saving || !session.is_admin || baseline === null) return
    saving = true
    const id = detail.id
    try {
      const metaChanged =
        baseline.name !== nameEdit || baseline.order_mode !== orderModeEdit || baseline.interval_s !== intervalEdit
      const orderChanged = serializeOrder(itemIds(items)) !== serializeOrder(baseline.item_ids)
      if (metaChanged) {
        await apiFetch(`/api/playlists/${id}`, {
          method: 'PATCH',
          body: JSON.stringify({ name: nameEdit.trim(), order_mode: orderModeEdit, interval_s: intervalEdit }),
        })
      }
      if (orderChanged && items.length > 0) {
        await apiFetch(`/api/playlists/${id}/items`, {
          method: 'PATCH',
          body: JSON.stringify({ item_ids: itemIds(items) }),
        })
      }
      clearDraft(`playlist:${id}`)
      notify.success(m['media.playlist.save_success']({ name: nameEdit.trim() }))
      await list.reload()
      await loadDetail(id)
    } catch (e) {
      notify.error(errText(e))
    } finally {
      saving = false
    }
  }

  function serializeOrder(ids: number[]): string {
    return ids.join(',')
  }

  async function doDelete(): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    if (!deleteArmed) {
      deleteArmed = true
      return
    }
    mutating = true
    const id = detail.id
    const name = detail.name
    try {
      await apiFetch(`/api/playlists/${id}`, { method: 'DELETE' })
      clearDraft(`playlist:${id}`)
      notify.success(m['media.playlist.delete_success']({ name }))
      selectedId = null
      detail = null
      baseline = null
      items = []
      serverItems = []
      await list.reload()
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
      deleteArmed = false
    }
  }

  async function doReshuffle(): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    try {
      await apiFetch(`/api/playlists/${detail.id}/reshuffle`, { method: 'POST' })
      notify.success(m['media.playlist.reshuffle_success']())
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
    }
  }

  async function addImage(imageId: number): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    const id = detail.id
    try {
      await apiFetch(`/api/playlists/${id}/items`, { method: 'POST', body: JSON.stringify({ image_id: imageId }) })
      notify.success(m['media.playlist.added_success']({ id: imageId }))
      await loadDetail(id) // refetch items + reset baseline; a pending reorder draft is preserved by id
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
    }
  }

  async function removeItem(itemId: number): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    const id = detail.id
    try {
      await apiFetch(`/api/playlists/${id}/items/${itemId}`, { method: 'DELETE' })
      notify.success(m['media.playlist.removed_success']())
      await loadDetail(id)
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
    }
  }

  async function bindDevice(): Promise<void> {
    if (!detail || mutating || !session.is_admin || !bindTarget) return
    mutating = true
    const serial = bindTarget
    try {
      await apiFetch(`/api/devices/${serial}/playlist`, {
        method: 'PUT',
        body: JSON.stringify({ playlist_id: detail.id }),
      })
      notify.success(m['media.playlist.bind_success']({ serial }))
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
    }
  }

  async function unbindDevice(): Promise<void> {
    if (!detail || mutating || !session.is_admin || !bindTarget) return
    mutating = true
    const serial = bindTarget
    try {
      await apiFetch(`/api/devices/${serial}/playlist`, { method: 'DELETE' })
      notify.success(m['media.playlist.unbind_success']({ serial }))
    } catch (e) {
      notify.error(errText(e))
    } finally {
      mutating = false
    }
  }

  // ---- drag reorder (HTML5 DnD; no dependency — lockfile is frozen) ----
  function onDragStart(i: number): void {
    dragFrom = i
  }
  function onDrop(i: number): void {
    if (dragFrom !== null && dragFrom !== i) items = reorder(items, dragFrom, i)
    dragFrom = null
  }
  function orderModeLabel(mode: OrderMode): string {
    return mode === 'shuffle' ? m['media.playlist.order_mode.shuffle']() : m['media.playlist.order_mode.sequential']()
  }
</script>

<div class="editor">
  <div class="list-col">
    <h2>{m['media.playlist.heading']()}</h2>
    <StateView resource={list} emptyText={m['media.playlist.empty']()} loadingText={m['media.playlist.loading']()}>
      {#snippet ready(playlists: PlaylistSummary[])}
        <ul class="pl-list" role="listbox">
          {#each playlists as pl (pl.id)}
            <li>
              <button
                type="button"
                role="option"
                class="pl-opt"
                class:selected={pl.id === selectedId}
                aria-selected={pl.id === selectedId}
                onclick={() => loadDetail(pl.id)}
              >
                <span class="pl-name">{pl.name}</span>
                <span class="pl-mode">{orderModeLabel(pl.order_mode)}</span>
                <span class="pl-ver">{m['media.playlist.version']({ version: pl.version })}</span>
              </button>
            </li>
          {/each}
        </ul>
        {#if list.moreError}
          <div class="more-error" role="alert">
            <p>{list.moreError.message}</p>
            <button onclick={() => list.loadMore()}>{m['media.playlist.retry_more']()}</button>
          </div>
        {:else if list.hasMore}
          <button class="load-more" onclick={() => list.loadMore()} disabled={list.loadingMore}>
            {list.loadingMore ? m['media.playlist.loading_more']() : m['media.playlist.load_more']()}
          </button>
        {/if}
      {/snippet}
    </StateView>

    {#if session.is_admin}
      <div class="create">
        <h3>{m['media.playlist.create_heading']()}</h3>
        <label>
          {m['media.playlist.name_label']()}
          <input
            type="text"
            bind:value={newName}
            placeholder={m['media.playlist.name_placeholder']()}
            autocomplete="off"
            spellcheck="false"
          />
        </label>
        <label>
          {m['media.playlist.order_mode_label']()}
          <select bind:value={newOrderMode}>
            {#each ORDER_MODES as mode (mode)}
              <option value={mode}>{orderModeLabel(mode)}</option>
            {/each}
          </select>
        </label>
        <label>
          {m['media.playlist.interval_label']()}
          <input type="number" min="1" bind:value={newInterval} />
        </label>
        <button
          type="button"
          class="primary"
          disabled={affordance.disabled || !createValid || creating}
          title={affordance.disabled ? affordance.title : ''}
          onclick={doCreate}
        >
          {creating ? m['media.playlist.creating']() : m['media.playlist.create']()}
        </button>
      </div>
    {/if}
  </div>

  <div class="detail-col">
    {#if detailStatus === 'loading'}
      <p class="muted" aria-busy="true">{m['media.playlist.items_loading']()}</p>
    {:else if detailStatus === 'error'}
      <div class="detail-error" role="alert">
        <p>{detailError}</p>
        {#if selectedId !== null}
          <button type="button" onclick={() => selectedId !== null && loadDetail(selectedId)}>
            {m['media.playlist.retry_more']()}
          </button>
        {/if}
      </div>
    {:else if detail}
      <div class="detail">
        <div class="detail-head">
          <h2>{detail.name}</h2>
          <span class="muted">{m['media.playlist.version']({ version: detail.version })}</span>
          {#if dirty}<span class="badge dirty">{m['media.playlist.dirty_badge']()}</span>{/if}
        </div>

        <div class="meta">
          <label>
            {m['media.playlist.name_label']()}
            <input type="text" bind:value={nameEdit} disabled={affordance.disabled} spellcheck="false" />
          </label>
          <label>
            {m['media.playlist.order_mode_label']()}
            <select bind:value={orderModeEdit} disabled={affordance.disabled}>
              {#each ORDER_MODES as mode (mode)}
                <option value={mode}>{orderModeLabel(mode)}</option>
              {/each}
            </select>
          </label>
          <label>
            {m['media.playlist.interval_label']()}
            <input type="number" min="1" bind:value={intervalEdit} disabled={affordance.disabled} />
          </label>
        </div>

        <div class="save-row">
          <button
            type="button"
            class="primary"
            disabled={affordance.disabled || saving || !dirty}
            title={affordance.disabled ? affordance.title : ''}
            onclick={doSave}
          >
            {saving ? m['media.playlist.saving']() : m['media.playlist.save']()}
          </button>
          <button
            type="button"
            disabled={affordance.disabled || mutating || orderModeEdit !== 'shuffle'}
            title={affordance.disabled ? affordance.title : ''}
            onclick={doReshuffle}
          >
            {m['media.playlist.reshuffle']()}
          </button>
          <button
            type="button"
            class="danger"
            disabled={affordance.disabled || mutating}
            title={affordance.disabled ? affordance.title : ''}
            onclick={doDelete}
          >
            {deleteArmed ? m['media.playlist.delete_confirm']() : m['media.playlist.delete']()}
          </button>
          {#if deleteArmed}
            <button type="button" class="link" onclick={() => (deleteArmed = false)}>
              {m['media.playlist.delete_cancel']()}
            </button>
          {/if}
        </div>

        <section class="items">
          <h3>{m['media.playlist.items_heading']()}</h3>
          {#if items.length === 0}
            <p class="muted small">{m['media.playlist.items_empty']()}</p>
          {:else}
            <p class="muted small">{m['media.playlist.drag_hint']()}</p>
            <ul class="item-list">
              {#each items as it, i (it.id)}
                <li
                  class="item"
                  class:dragging={dragFrom === i}
                  draggable={session.is_admin}
                  ondragstart={() => onDragStart(i)}
                  ondragover={(e) => e.preventDefault()}
                  ondrop={(e) => {
                    e.preventDefault()
                    onDrop(i)
                  }}
                  ondragend={() => (dragFrom = null)}
                >
                  <span class="pos">{i + 1}</span>
                  <img
                    class="thumb"
                    src={`/api/images/${it.image_id}/thumbnail`}
                    alt={m['media.playlist.item_thumb_alt']({ id: it.image_id })}
                    loading="lazy"
                    width="80"
                    height="60"
                  />
                  <span class="item-meta">{it.fit} · {it.dither}</span>
                  {#if session.is_admin}
                    <button type="button" class="item-x" disabled={mutating} onclick={() => removeItem(it.id)}>
                      {m['media.playlist.item_remove']()}
                    </button>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
        </section>

        {#if session.is_admin}
          <section class="add">
            <h3>{m['media.playlist.add_heading']()}</h3>
            <StateView resource={library} emptyText={m['media.playlist.add_empty']()}>
              {#snippet ready(images: Image[])}
                <ul class="add-grid">
                  {#each images as img (img.id)}
                    <li>
                      <button type="button" class="add-tile" disabled={mutating} onclick={() => addImage(img.id)}>
                        <img
                          class="thumb"
                          src={`/api/images/${img.id}/thumbnail`}
                          alt={m['media.playlist.item_thumb_alt']({ id: img.id })}
                          loading="lazy"
                          width="80"
                          height="60"
                        />
                        <span class="add-label">{m['media.playlist.add']()}</span>
                      </button>
                    </li>
                  {/each}
                </ul>
                {#if library.hasMore}
                  <button class="load-more" onclick={() => library.loadMore()} disabled={library.loadingMore}>
                    {library.loadingMore ? m['media.playlist.loading_more']() : m['media.playlist.load_more']()}
                  </button>
                {/if}
              {/snippet}
            </StateView>
          </section>

          <section class="bind">
            <h3>{m['media.playlist.bind_heading']()}</h3>
            <p class="muted small">{m['media.playlist.bind_none']()}</p>
            <div class="bind-row">
              <DevicePicker devices={deviceList} bind:value={bindTarget} />
              <button type="button" disabled={mutating || !bindTarget} onclick={bindDevice}>
                {m['media.playlist.bind']()}
              </button>
              <button type="button" disabled={mutating || !bindTarget} onclick={unbindDevice}>
                {m['media.playlist.unbind']()}
              </button>
            </div>
          </section>
        {:else}
          <p class="muted small">{m['media.playlist.bind_admin_only']()}</p>
        {/if}
      </div>
    {:else}
      <p class="muted select-hint">{m['media.playlist.select_hint']()}</p>
    {/if}
  </div>
</div>

<style>
  .editor {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.9fr);
    gap: 1rem;
    align-items: start;
  }
  .list-col,
  .detail-col {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    min-width: 0;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  h3 {
    margin: 0 0 0.25rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  .muted {
    color: var(--fg-muted);
    margin: 0;
  }
  .small {
    font-size: 0.8rem;
  }
  .pl-list {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: 6px;
    max-height: 20rem;
    overflow-y: auto;
  }
  .pl-opt {
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
  .pl-opt:hover {
    background: rgba(255, 255, 255, 0.04);
  }
  .pl-opt.selected {
    background: rgba(122, 162, 247, 0.14);
  }
  .pl-name {
    flex: 1;
    font-weight: 600;
  }
  .pl-mode,
  .pl-ver {
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .create,
  .meta {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .create {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
  }
  .meta {
    flex-direction: row;
    flex-wrap: wrap;
    gap: 0.6rem;
    align-items: flex-end;
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
  }
  input,
  select {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.35rem 0.5rem;
    font-size: 0.88rem;
  }
  .meta input[type='number'] {
    width: 6rem;
  }
  input:disabled,
  select:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.3rem 0.9rem;
    cursor: pointer;
    font-size: 0.85rem;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  button:disabled {
    color: var(--fg-muted);
    cursor: not-allowed;
  }
  button.primary {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--bg, #0b0f17);
    font-weight: 600;
  }
  button.primary:disabled {
    background: transparent;
    color: var(--fg-muted);
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  button.link {
    border: none;
    padding: 0.3rem 0.3rem;
    color: var(--fg-muted);
  }
  .detail {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.75rem;
  }
  .detail-head {
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.4rem;
  }
  .detail-head h2 {
    font-size: 1.15rem;
  }
  .badge {
    font-size: 0.7rem;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--fg-muted);
  }
  .badge.dirty {
    color: var(--warn, #d08770);
    border-color: var(--warn, #d08770);
  }
  .save-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .items,
  .add,
  .bind {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .item-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .item {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.3rem 0.5rem;
    background: var(--bg-muted, rgba(127, 127, 127, 0.06));
    cursor: grab;
  }
  .item.dragging {
    opacity: 0.5;
    border-color: var(--accent);
  }
  .pos {
    font-variant-numeric: tabular-nums;
    color: var(--fg-muted);
    width: 1.5rem;
    text-align: right;
  }
  .thumb {
    object-fit: contain;
    image-rendering: pixelated;
    border-radius: 4px;
    background: var(--bg);
  }
  .item-meta {
    flex: 1;
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .item-x {
    font-size: 0.75rem;
    padding: 0.1rem 0.5rem;
  }
  .add-grid {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(90px, 1fr));
    gap: 0.5rem;
  }
  .add-tile {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 0.2rem;
    width: 100%;
    padding: 0.3rem;
  }
  .add-label {
    font-size: 0.72rem;
  }
  .load-more {
    align-self: flex-start;
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
  .detail-error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    align-items: flex-start;
  }
  .detail-error p {
    margin: 0;
    color: var(--danger);
  }
  .bind-row {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .select-hint {
    padding: 0.5rem 0;
  }
</style>
