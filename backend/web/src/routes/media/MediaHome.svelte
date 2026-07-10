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
  import { apiFetch } from '../../lib/api'
  import { Paged } from '../../lib/media/paged.svelte'
  import Uploader from '../../lib/media/Uploader.svelte'
  import type { Image, ImagesResponse } from '../../lib/media/types'
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
  onMount(() => library.reload())
</script>

<section class="media">
  <header>
    <h1>{m['media.title']()}</h1>
    <p class="subtitle">{m['media.subtitle']()}</p>
  </header>

  <Uploader onUploaded={() => library.reload()} />

  <section class="library" aria-label={m['media.library.heading']()}>
    <h2>{m['media.library.heading']()}</h2>
    <StateView resource={library} emptyText={m['media.library.empty']()}>
      {#snippet ready(images)}
        <ul class="grid">
          {#each images as image (image.id)}
            <li class="tile">
              <img
                class="thumb"
                src={`/api/images/${image.id}/thumbnail`}
                alt={m['media.library.thumb_alt']({ id: image.id })}
                loading="lazy"
                width="160"
                height="120"
              />
              <span class="dims">{image.width}×{image.height}</span>
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
