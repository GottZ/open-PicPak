<script lang="ts">
  // Channels panel (W3, design 01-ota-spa §4.3/§6): list of channels
  // (GET /api/channels, write.go:161-164) with a "Standard setzen" control per
  // row, admin-gated (§5 B1). The version <select> needs the firmware
  // inventory the design calls "von OtaHome hochgereicht" (§4.3).
  //
  // STRUCTURE DECISION (documented per task brief, weighed against lifting the
  // Resource into OtaHome + prop-passing to both panels): ChannelPanel loads
  // its OWN Resource<FirmwareResponse> instead. Reasoning — OtaHome mounts
  // panels through `{#if activeTab === …}` (OtaHome.svelte:65-71), so
  // switching tabs UNMOUNTS/remounts the panel component (the same unmount
  // OtaHome's W2 comment names for the upload-in-flight guard); `onMount`
  // re-fires every time the operator opens the Channels tab, so a self-loaded
  // Resource is exactly as fresh after a Firmware upload as a lifted-and-
  // reloaded one would be — the operator can only reach a freshly-uploaded
  // version by switching TO this tab, which is precisely when this Resource
  // (re)loads. Lifting would only pay off if both panels needed to stay
  // mounted and reactively in sync SIMULTANEOUSLY, which the tab shell never
  // does, and it would force a signature change onto the already-committed
  // (A2) FirmwarePanel. Self-loading keeps W3 a pure addition: zero edits to
  // FirmwarePanel/OtaHome's data flow, one new route wired in OtaHome.
  //
  // B6 (§5): `version`/channel `name` are free/DB-sourced strings; every value
  // below renders as a text node ({…}), never {@html} — pinned in ota.test.ts.
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { apiFetch } from '../../lib/api'
  import { Resource } from '../../lib/resource.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import { otaErrorText } from '../../lib/ota/firmware'
  import type { ChannelRow, ChannelsResponse, FirmwareResponse } from '../../lib/ota/types'
  import { m } from '../../paraglide/messages.js'

  const channels = new Resource<ChannelsResponse>(() => apiFetch<ChannelsResponse>('/api/channels'))
  const firmware = new Resource<FirmwareResponse>(() => apiFetch<FirmwareResponse>('/api/firmware'))

  const affordance = $derived(mutationAffordance(session.is_admin))

  // §6: firmware_versions is append-only/unbounded (no DELETE route,
  // ota_http.go:40-48) — never render the full list into a <select>. Default
  // to the latest N (the API already orders created_at DESC, write.go:144),
  // with a manual "ältere zeigen" expand toggle (§6, i18n gap filled this
  // wave: ota.channel.versions_older/_recent — no prior key covered it).
  const VERSION_DISPLAY_LIMIT = 20
  let showOlder = $state(false)
  const allVersions = $derived(firmware.data?.firmware ?? [])
  const visibleVersions = $derived(showOlder ? allVersions : allVersions.slice(0, VERSION_DISPLAY_LIMIT))
  const hasOlder = $derived(allVersions.length > VERSION_DISPLAY_LIMIT)

  // Per-channel <select> value, keyed by channel name; only populated once the
  // operator picks something (uncontrolled default via versionFor below), so
  // a reload() never clobbers a pending, not-yet-submitted choice.
  let picked = $state<Record<string, string>>({})
  let setting = $state<string | null>(null) // channel name currently mid-PUT, or null

  /** The select's effective value: explicit pick > current channel default > newest visible version. */
  function versionFor(ch: ChannelRow): string {
    return picked[ch.name] ?? ch.default_version ?? visibleVersions[0]?.version ?? ''
  }

  function onPick(name: string, e: Event): void {
    picked[name] = (e.currentTarget as HTMLSelectElement).value
  }

  // §4.3 setDefault skeleton, verbatim mechanism: Handler-Guard VOR dem Call,
  // zusätzlich zur (kosmetischen) Affordance (§5 B1) — the server enforces
  // RequireAdmin (403) regardless; this guard is belt-and-suspenders, not the
  // gate.
  async function setDefault(name: string, version: string): Promise<void> {
    if (!session.is_admin || setting !== null || version === '') return
    setting = name
    try {
      await apiFetch(`/api/channels/${name}`, { method: 'PUT', body: JSON.stringify({ version }) })
      notify.success(m['ota.channel.default_set']({ name, version }))
      await channels.reload()
    } catch (err) {
      notify.error(otaErrorText(err)) // unknown_version 422 / not_found 404, §4.3
    } finally {
      setting = null
    }
  }

  onMount(() => {
    void channels.load()
    void firmware.load()
  })
</script>

<section class="channels" aria-label={m['ota.channel.heading']()}>
  <h2>{m['ota.channel.heading']()}</h2>

  {#if hasOlder}
    <button type="button" class="toggle" onclick={() => (showOlder = !showOlder)}>
      {showOlder ? m['ota.channel.versions_recent']() : m['ota.channel.versions_older']()}
    </button>
  {/if}

  <StateView resource={channels} isEmpty={(data) => data.channels.length === 0}>
    {#snippet ready(data)}
      <table>
        <thead>
          <tr>
            <th>{m['ota.channel.heading']()}</th>
            <th>{m['ota.fw.col.version']()}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {#each data.channels as ch (ch.name)}
            <tr>
              <td>{ch.name}</td>
              <td>{ch.default_version ?? m['ota.channel.default_none']()}</td>
              <td class="actions">
                <select
                  value={versionFor(ch)}
                  onchange={(e) => onPick(ch.name, e)}
                  disabled={affordance.disabled || setting === ch.name || visibleVersions.length === 0}
                  aria-label={m['ota.fw.col.version']()}
                >
                  {#each visibleVersions as v (v.version)}
                    <option value={v.version}>{v.version}</option>
                  {/each}
                </select>
                <button
                  type="button"
                  onclick={() => setDefault(ch.name, versionFor(ch))}
                  disabled={affordance.disabled || setting === ch.name || versionFor(ch) === ''}
                  title={affordance.disabled ? affordance.title : ''}
                  aria-disabled={affordance['aria-disabled']}
                >
                  {m['ota.channel.set']()}
                </button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/snippet}
  </StateView>
</section>

<style>
  .channels {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .toggle {
    align-self: flex-start;
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-muted);
    border-radius: 6px;
    padding: 0.2rem 0.7rem;
    cursor: pointer;
    font-size: 0.8rem;
  }
  .toggle:hover {
    color: var(--fg);
    border-color: var(--accent);
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.875rem;
  }
  th,
  td {
    text-align: left;
    padding: 0.4rem 0.6rem;
    border-bottom: 1px solid var(--border);
  }
  th {
    color: var(--fg-muted);
    font-weight: 600;
    font-size: 0.8rem;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  select {
    font: inherit;
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.3rem 0.8rem;
    cursor: pointer;
    font-size: 0.85rem;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  button:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
</style>
