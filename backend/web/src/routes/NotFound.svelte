<script lang="ts">
  import { m } from '../paraglide/messages.js'
</script>

<section class="not-found">
  <p class="code">404</p>
  <p>{m['notfound.message']()}</p>
  <p><a href="/fleet">{m['notfound.back']()}</a></p>
</section>

<style>
  .not-found {
    display: grid;
    justify-items: start;
    gap: 0.5rem;
    max-width: 44rem;
  }
  .code {
    margin: 0;
    font-family: monospace;
    font-size: 2rem;
    color: var(--fg-muted);
  }
  p {
    margin: 0;
  }
</style>
