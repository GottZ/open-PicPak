<script lang="ts">
  // /ota operator area shell (design 01-ota-spa §4, Muster MediaHome.svelte:50,
  // 170-196): EIN Route-Slot, in-Shell-Tabs statt Route-Split, damit Firmware,
  // Channels und Rollouts unter der einen /ota-Area bleiben. W1 mountete nur den
  // Firmware-Tab; W3 (diese Welle) hängt ChannelPanel ein. Rollouts existiert
  // weiterhin sichtbar als neutraler Platzhalter, bis sein Panel (W4) landet.
  //
  // W2 Tab-Wechsel-Kante (§4.2): a tab click here is a LOCAL {#if}-render swap,
  // not an sv-router navigation — FirmwarePanel's useDirtyGuard (blockNavigation)
  // covers a real route-away / tab-close but NOT this in-shell switch, so an
  // upload mid-flight would be silently unmounted (XHR aborted) by a bare
  // activeTab reassignment. `uploading` is lifted here via a $bindable prop and
  // the Channels/Rollouts tab buttons are disabled-with-reason while it is true —
  // the same disabled-with-reason discipline mutationAffordance uses elsewhere,
  // applied to navigation instead of a mutation. This same swap is what lets
  // ChannelPanel (W3) self-load its firmware Resource instead of taking it as a
  // prop — the {#if}-swap unmounts/remounts the panel on every tab entry, so
  // onMount already re-fires fresh each time (see ChannelPanel.svelte header).
  import FirmwarePanel from './FirmwarePanel.svelte'
  import ChannelPanel from './ChannelPanel.svelte'
  import { m } from '../../paraglide/messages.js'

  let activeTab = $state<'firmware' | 'channels' | 'rollouts'>('firmware')
  let firmwareUploading = $state(false)
</script>

<section class="ota">
  <header>
    <h1>{m['ota.title']()}</h1>
    <p class="subtitle">{m['ota.subtitle']()}</p>
    <div class="tabs" role="tablist" aria-label={m['ota.title']()}>
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={activeTab === 'firmware'}
        aria-selected={activeTab === 'firmware'}
        onclick={() => (activeTab = 'firmware')}
      >
        {m['ota.tab.firmware']()}
      </button>
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={activeTab === 'channels'}
        aria-selected={activeTab === 'channels'}
        disabled={firmwareUploading}
        aria-disabled={firmwareUploading}
        onclick={() => (activeTab = 'channels')}
      >
        {m['ota.tab.channels']()}
      </button>
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={activeTab === 'rollouts'}
        aria-selected={activeTab === 'rollouts'}
        disabled={firmwareUploading}
        aria-disabled={firmwareUploading}
        onclick={() => (activeTab = 'rollouts')}
      >
        {m['ota.tab.rollouts']()}
      </button>
    </div>
  </header>

  {#if activeTab === 'firmware'}
    <FirmwarePanel bind:uploading={firmwareUploading} />
  {:else if activeTab === 'channels'}
    <ChannelPanel />
  {:else}
    <!-- Rollouts lands in W4 (design 01-ota-spa §7) — a neutral pending
         state, never a blank tab. -->
    <p class="state muted">{m['area.pending']()}</p>
  {/if}
</section>

<style>
  .ota {
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
  .tab:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .tab:hover {
    color: var(--fg);
  }
  .tab.active {
    color: var(--fg);
    border-color: var(--accent);
    background: rgba(122, 162, 247, 0.1);
  }
  .state {
    margin: 0;
  }
  .muted {
    color: var(--fg-muted);
  }
</style>
