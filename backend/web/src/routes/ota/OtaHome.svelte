<script lang="ts">
  // /ota operator area shell (design 01-ota-spa §4, Muster MediaHome.svelte:50,
  // 170-196): EIN Route-Slot, in-Shell-Tabs statt Route-Split, damit Firmware,
  // Channels und Rollouts unter der einen /ota-Area bleiben. W1 mountet nur den
  // Firmware-Tab (Liste read-only); Channels/Rollouts existieren sichtbar als
  // neutraler Platzhalter, bis ihre Panels (W3/W4) landen.
  import FirmwarePanel from './FirmwarePanel.svelte'
  import { m } from '../../paraglide/messages.js'

  let activeTab = $state<'firmware' | 'channels' | 'rollouts'>('firmware')
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
        onclick={() => (activeTab = 'rollouts')}
      >
        {m['ota.tab.rollouts']()}
      </button>
    </div>
  </header>

  {#if activeTab === 'firmware'}
    <FirmwarePanel />
  {:else}
    <!-- Channels/Rollouts land in W3/W4 (design 01-ota-spa §7) — a neutral
         pending state, never a blank tab. -->
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
