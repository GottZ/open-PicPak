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
  import { m } from '../../paraglide/messages.js'

  // Placeholder row until lib/media/types.ts (design 29 §7 W4) lands the real
  // Image wire type; the paged list state (§6/§7 W5) then replaces the Resource.
  type LibraryRow = { id: number }
  const library = new Resource<LibraryRow[]>(async () => [])
  onMount(() => library.load())
</script>

<section class="media">
  <header>
    <h1>{m['media.title']()}</h1>
    <p class="subtitle">{m['media.subtitle']()}</p>
  </header>

  <section class="library" aria-label={m['media.library.heading']()}>
    <h2>{m['media.library.heading']()}</h2>
    <StateView resource={library} emptyText={m['media.library.empty']()}>
      {#snippet ready(images)}
        <ul class="grid">
          {#each images as image (image.id)}
            <li class="tile">{image.id}</li>
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
    display: grid;
    place-items: center;
    color: var(--fg-muted);
  }
</style>
