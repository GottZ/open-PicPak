<script lang="ts">
  // Shell-wide live-connection indicator (D19.14). Reads the global conn store
  // (the active page's EventsClient mirrors its status there) so an operator
  // always knows when live views are stale because the stream dropped.
  import { conn, connDisplay } from './conn.svelte'
  import { m } from '../paraglide/messages.js'

  const d = $derived(connDisplay(conn.status))
</script>

<span class="conn conn-{d.tone}" title={m['conn.title']()}>
  <span class="dot" aria-hidden="true"></span>{d.label}
</span>

<style>
  .conn {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    font-family: monospace;
    font-size: 0.7rem;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--fg-muted);
  }
  .dot {
    width: 7px;
    height: 7px;
    border-radius: 999px;
    background: currentColor;
  }
  .conn-live {
    color: var(--ok);
  }
  .conn-reconnecting {
    color: var(--warn);
  }
  .conn-offline {
    color: var(--fg-muted);
  }
</style>
