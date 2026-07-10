<script lang="ts">
  import { route } from '../router'
  import { AREAS } from './index'
  import { m } from '../paraglide/messages.js'
  import { navLabel, areaShips } from '../lib/i18n'

  // Generic slot for the not-yet-built areas (design 19 §4.4): nav + routing are
  // real, the target UI ships in a later design doc. sv-router can't pass props
  // to a lazy route, so the area is derived from the live pathname against AREAS.
  const area = $derived(AREAS.find((a) => a.path === route.pathname))
</script>

<section class="area">
  <header>
    <h1>{area ? navLabel(area.path) : ''}</h1>
    <span class="pending">{m['area.pending']()}</span>
  </header>
  <p class="description">
    {m['area.reserved']()}
  </p>
  {#if area}
    <p class="ships">{areaShips(area.path)}</p>
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    max-width: 44rem;
  }
  header {
    display: flex;
    align-items: baseline;
    gap: 1rem;
    border-bottom: 1px solid #2a2a32;
    padding-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
  }
  .pending {
    font-family: monospace;
    font-size: 0.7rem;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--fg-muted);
    border: 1px dashed #3a3a44;
    border-radius: 4px;
    padding: 0.05rem 0.5rem;
  }
  .description {
    margin: 0;
  }
  .ships {
    margin: 0;
    color: var(--fg-muted);
    font-size: 0.875rem;
  }
</style>
