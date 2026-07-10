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
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import Uploader from '../../lib/media/Uploader.svelte'
  import type { Image, ImagesResponse } from '../../lib/media/types'
  import { m } from '../../paraglide/messages.js'

  // W4 lands the real Image wire type + a single-shot GET /api/images Resource so an
  // upload can refresh the library; W5 (§6/§7) swaps this for the paged accumulator
  // (lib/media/paged.svelte.ts) that the target-scale grid needs. Only the first
  // cursor page is read here — the paged grid is out of W4 scope.
  const library = new Resource<Image[]>(async () => {
    const res = await apiFetch<ImagesResponse>('/api/images')
    return res.images
  })
  onMount(() => library.load())
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
              <span class="dims">{image.width}×{image.height}</span>
              <span class="mime">{image.mime}</span>
            </li>
          {/each}
        </ul>
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
    border: 1px solid var(--border);
    border-radius: 6px;
    aspect-ratio: 4 / 3;
    display: flex;
    flex-direction: column;
    justify-content: center;
    align-items: center;
    gap: 0.25rem;
    color: var(--fg-muted);
    font-size: 0.8rem;
  }
  .tile .mime {
    font-size: 0.7rem;
    opacity: 0.75;
  }
</style>
