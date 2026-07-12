<script lang="ts">
  // Rollouts panel (W4, design 01-ota-spa §4.4/§6/§7): list (GET /api/rollouts,
  // channel-grouped, collapsed by default), upsert (fleet '*' or a per-serial
  // pin), state active/paused/done, armed two-step delete, admin-gated (§5 B1).
  // Reads are Auth-only backend policy (D20.1); mutationAffordance mirrors it
  // cosmetically, the server stays authoritative.
  //
  // §6 DOM budget: `rollout_targets` is UNGEPAGT and can reach ~1000 rows at
  // fleet scale (500 devices × 2 channels). Rendering all of them × 4 buttons
  // each would put ~4000 interactive nodes in the DOM at once — the same class
  // of problem MediaHome's IntersectionObserver virtualisation solves for images.
  // Here the fix is coarser but sufficient: channel groups render collapsed by
  // default (header + count only); a group's rows are MOUNTED, not just hidden,
  // only once its {#if expanded[...]} is true — an unexpanded group costs one
  // button, zero table rows.
  //
  // B6 (§5): `serial`/`channel`/`version` are free/DB-sourced strings; every
  // value below renders as a text node ({…}), never {@html} — pinned in
  // ota.test.ts.
  import { onMount, onDestroy } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import { apiFetch } from '../../lib/api'
  import { EventsClient } from '../../lib/events.svelte'
  import { conn } from '../../lib/conn.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import { otaErrorText } from '../../lib/ota/firmware'
  import { isRolloutGone, isValidRolloutSerial, rolloutErrorText } from '../../lib/ota/rollouts'
  import type {
    ChannelsResponse,
    FirmwareResponse,
    RolloutRow,
    RolloutsResponse,
  } from '../../lib/ota/types'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import { m } from '../../paraglide/messages.js'

  const rollouts = new Resource<RolloutsResponse>(() => apiFetch<RolloutsResponse>('/api/rollouts'))
  const channels = new Resource<ChannelsResponse>(() => apiFetch<ChannelsResponse>('/api/channels'))
  const firmware = new Resource<FirmwareResponse>(() => apiFetch<FirmwareResponse>('/api/firmware'))

  // A6 (E2): live `ota` reload-hint, same lifecycle as FirmwarePanel/ChannelPanel
  // (created in onMount, closed in onDestroy — the in-shell tab swap unmounts
  // this panel, OtaHome.svelte {#if}).
  let events = $state<EventsClient | null>(null)

  $effect(() => {
    conn.status = events?.status ?? 'idle'
  })

  const affordance = $derived(mutationAffordance(session.is_admin))

  // §6: firmware_versions is append-only/unbounded — mirror ChannelPanel's
  // latest-N toggle LOCALLY (deliberately not extracted into a shared
  // component, per the task brief: "spiegel das Muster lokal schlicht").
  const VERSION_DISPLAY_LIMIT = 20
  let showOlderVersions = $state(false)
  const allVersions = $derived(firmware.data?.firmware ?? [])
  const visibleVersions = $derived(
    showOlderVersions ? allVersions : allVersions.slice(0, VERSION_DISPLAY_LIMIT),
  )
  const hasOlderVersions = $derived(allVersions.length > VERSION_DISPLAY_LIMIT)

  // §6: channel-grouped, collapsed-by-default rows (see header comment).
  interface ChannelGroup {
    channel: string
    rows: RolloutRow[]
  }
  const groups = $derived.by((): ChannelGroup[] => {
    const rows = rollouts.data?.rollouts ?? []
    const byChannel = new Map<string, RolloutRow[]>()
    for (const row of rows) {
      const bucket = byChannel.get(row.channel)
      if (bucket) bucket.push(row)
      else byChannel.set(row.channel, [row])
    }
    return [...byChannel.entries()].map(([channel, groupRows]) => ({ channel, rows: groupRows }))
  })

  let groupExpanded = $state<Record<string, boolean>>({})
  function toggleGroup(channel: string): void {
    groupExpanded[channel] = !groupExpanded[channel]
  }

  // §6: device roster feeds both DevicePicker surfaces below (upsert target +
  // list filter) — same raw-apiFetch-into-array pattern as MediaHome's panel
  // picker (no Resource wrapper: a failed fetch just leaves both pickers empty).
  let deviceList = $state<Device[]>([])

  // §6 "Default-Serial-Filter via DevicePicker": narrows every group's visible
  // rows to a single serial (the fleet '*' row always stays alongside a match,
  // since it also applies to that device) — a lookup aid over the DOM budget.
  // Picking a filter auto-expands every group that actually contains a match,
  // so the operator does not have to hunt for it group by group.
  let filterSerial = $state<string | null>(null)
  function rowsFor(group: ChannelGroup): RolloutRow[] {
    if (filterSerial === null) return group.rows
    return group.rows.filter((r) => r.serial === filterSerial || r.serial === '*')
  }
  $effect(() => {
    if (filterSerial === null) return
    for (const group of groups) {
      if (group.rows.some((r) => r.serial === filterSerial || r.serial === '*')) {
        groupExpanded[group.channel] = true
      }
    }
  })

  function stateLabel(state: RolloutRow['state']): string {
    switch (state) {
      case 'active':
        return m['ota.rollout.state.active']()
      case 'paused':
        return m['ota.rollout.state.paused']()
      case 'done':
        return m['ota.rollout.state.done']()
    }
  }

  // --- Upsert (§4.4): Fleet '*' vs per-serial pin (Canary-Modell, E1=VOLL) ---
  let targetMode = $state<'fleet' | 'device'>('fleet')
  let targetSerial = $state<string | null>(null)
  let formChannel = $state('')
  let formVersion = $state('')
  let submitting = $state(false)

  const effectiveSerial = $derived(targetMode === 'fleet' ? '*' : (targetSerial ?? ''))
  // Client-Validierung spiegelt ota_http.go:168-175 (§4.4): serial '*' oder die
  // Regex, channel+version Pflicht — sonst kein Wire-Roundtrip für offensichtlich
  // Invalides.
  const canSubmit = $derived(
    !submitting &&
      !affordance.disabled &&
      isValidRolloutSerial(effectiveSerial) &&
      formChannel !== '' &&
      formVersion !== '',
  )

  async function submitUpsert(e: Event): Promise<void> {
    e.preventDefault()
    if (!session.is_admin || !canSubmit) return // Handler-Guard VOR dem Call, zusätzlich zur Affordance (§5 B1)
    submitting = true
    try {
      await apiFetch('/api/rollouts', {
        method: 'POST',
        body: JSON.stringify({ serial: effectiveSerial, channel: formChannel, version: formVersion }),
      })
      // Upsert forciert state='active' (ON CONFLICT re-aktiviert eine paused/done-
      // Zeile, write.go:100-111, §4.4) — "reaktiviert" passt für eine neue wie eine
      // wiederbelebte Zeile gleichermaßen; die Response unterscheidet die zwei
      // Fälle nicht, also gibt es keinen sinnvolleren, response-abgeleiteten Text.
      notify.success(m['ota.rollout.reactivated']())
      targetMode = 'fleet'
      targetSerial = null
      formChannel = ''
      formVersion = ''
      await rollouts.reload()
    } catch (err) {
      notify.error(rolloutErrorText(err)) // unknown_serial / unknown_channel_or_version (§4.4)
    } finally {
      submitting = false
    }
  }

  // --- State-Wechsel (§4.4, PATCH /api/rollouts/{id}) ---
  let settingId = $state<number | null>(null)

  async function setRolloutState(id: number, state: RolloutRow['state']): Promise<void> {
    if (!session.is_admin || settingId !== null) return // Handler-Guard (§5 B1)
    settingId = id
    try {
      await apiFetch(`/api/rollouts/${id}`, { method: 'PATCH', body: JSON.stringify({ state }) })
      await rollouts.reload()
    } catch (err) {
      // 404-gone (§4.4): a second operator's row disappeared since the last list
      // load — the `ota` SSE hint (E2/A6) converges best-effort, not
      // transactionally. Localized + reload — never the generic otaErrorText
      // not_found (that string is "Channel nicht gefunden").
      if (isRolloutGone(err)) {
        notify.error(m['ota.rollout.error.gone']())
        await rollouts.reload()
      } else {
        notify.error(otaErrorText(err))
      }
    } finally {
      settingId = null
    }
  }

  // --- Delete (§4.4, MediaHome two-step armed pattern, MediaHome.svelte:78-104) ---
  let armed = $state<number | null>(null)
  let deleting = $state(false)

  async function doDelete(id: number): Promise<void> {
    if (!session.is_admin || deleting) return // Handler-Guard (§5 B1)
    if (armed !== id) {
      armed = id // first click arms; a second click on the SAME row commits.
      return
    }
    deleting = true
    try {
      await apiFetch(`/api/rollouts/${id}`, { method: 'DELETE' })
      await rollouts.reload()
    } catch (err) {
      // 404-gone (§4.4): already deleted by another operator — matches the
      // operator's own intent, so this reloads STILLSCHWEIGEND (no error toast),
      // unlike the state-change gone path above which does localize+toast.
      if (isRolloutGone(err)) {
        await rollouts.reload()
      } else {
        notify.error(otaErrorText(err))
      }
    } finally {
      deleting = false
      armed = null
    }
  }

  onMount(() => {
    void rollouts.load()
    void channels.load()
    void firmware.load()
    void apiFetch<DevicesResponse>('/api/devices')
      .then((r) => (deviceList = r.devices))
      .catch(() => (deviceList = []))
    events = new EventsClient({
      onOta: (hint) => {
        if (hint.kind === 'rollout') void rollouts.reload()
        if (hint.kind === 'channel') void channels.reload()
        if (hint.kind === 'firmware') void firmware.reload()
      },
    })
    void events.connect()
  })

  onDestroy(() => {
    events?.close()
    conn.status = 'idle'
  })
</script>

<section class="rollouts" aria-label={m['ota.rollout.heading']()}>
  <h2>{m['ota.rollout.heading']()}</h2>

  <form class="upsert" onsubmit={submitUpsert}>
    <fieldset class="target" disabled={affordance.disabled}>
      <legend>{m['ota.rollout.heading']()}</legend>
      <label class="radio">
        <input
          type="radio"
          name="rollout-target-mode"
          checked={targetMode === 'fleet'}
          onchange={() => (targetMode = 'fleet')}
        />
        {m['ota.rollout.target.fleet']()}
      </label>
      <label class="radio">
        <input
          type="radio"
          name="rollout-target-mode"
          checked={targetMode === 'device'}
          onchange={() => (targetMode = 'device')}
        />
        {m['ota.rollout.target.device']()}
      </label>
    </fieldset>

    {#if targetMode === 'device'}
      <DevicePicker devices={deviceList} bind:value={targetSerial} />
    {/if}

    <label class="field">
      <span class="field-label">{m['ota.rollout.channel']()}</span>
      <select bind:value={formChannel} disabled={affordance.disabled} aria-label={m['ota.rollout.channel']()}>
        <option value="">—</option>
        {#each channels.data?.channels ?? [] as ch (ch.name)}
          <option value={ch.name}>{ch.name}</option>
        {/each}
      </select>
    </label>

    <label class="field">
      <span class="field-label">{m['ota.rollout.version']()}</span>
      <select bind:value={formVersion} disabled={affordance.disabled} aria-label={m['ota.rollout.version']()}>
        <option value="">—</option>
        {#each visibleVersions as v (v.version)}
          <option value={v.version}>{v.version}</option>
        {/each}
      </select>
    </label>
    {#if hasOlderVersions}
      <button type="button" class="toggle" onclick={() => (showOlderVersions = !showOlderVersions)}>
        {showOlderVersions ? m['ota.channel.versions_recent']() : m['ota.channel.versions_older']()}
      </button>
    {/if}

    <button
      type="submit"
      disabled={!canSubmit}
      title={affordance.disabled ? affordance.title : ''}
      aria-disabled={affordance['aria-disabled']}
    >
      {m['ota.rollout.create']()}
    </button>
  </form>

  <div class="filter">
    <DevicePicker devices={deviceList} bind:value={filterSerial} />
  </div>

  <StateView resource={rollouts} emptyText={m['ota.rollout.empty']()} isEmpty={(data) => data.rollouts.length === 0}>
    {#snippet ready(_data)}
      <div class="groups">
        {#each groups as group (group.channel)}
          <div class="group">
            <button
              type="button"
              class="group-header"
              aria-expanded={groupExpanded[group.channel] ?? false}
              onclick={() => toggleGroup(group.channel)}
            >
              <span class="chevron" class:open={groupExpanded[group.channel]}>▶</span>
              <span class="group-name">{group.channel}</span>
              <span class="group-count">{group.rows.length}</span>
            </button>

            {#if groupExpanded[group.channel]}
              <table>
                <thead>
                  <tr>
                    <th>{m['ota.rollout.col.serial']()}</th>
                    <th>{m['ota.rollout.col.version']()}</th>
                    <th>{m['ota.rollout.col.state']()}</th>
                    <th>{m['ota.rollout.col.updated']()}</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {#each rowsFor(group) as row (row.id)}
                    <tr>
                      <td>{row.serial === '*' ? m['ota.rollout.target.fleet']() : row.serial}</td>
                      <td>{row.version}</td>
                      <td class="state-cell">
                        <button
                          type="button"
                          class="state-btn"
                          class:current={row.state === 'active'}
                          disabled={affordance.disabled || settingId === row.id || row.state === 'active'}
                          title={affordance.disabled ? affordance.title : ''}
                          aria-disabled={affordance['aria-disabled']}
                          onclick={() => setRolloutState(row.id, 'active')}
                        >
                          {stateLabel('active')}
                        </button>
                        <button
                          type="button"
                          class="state-btn"
                          class:current={row.state === 'paused'}
                          disabled={affordance.disabled || settingId === row.id || row.state === 'paused'}
                          title={affordance.disabled ? affordance.title : ''}
                          aria-disabled={affordance['aria-disabled']}
                          onclick={() => setRolloutState(row.id, 'paused')}
                        >
                          {stateLabel('paused')}
                        </button>
                        <button
                          type="button"
                          class="state-btn"
                          class:current={row.state === 'done'}
                          disabled={affordance.disabled || settingId === row.id || row.state === 'done'}
                          title={affordance.disabled ? affordance.title : ''}
                          aria-disabled={affordance['aria-disabled']}
                          onclick={() => setRolloutState(row.id, 'done')}
                        >
                          {stateLabel('done')}
                        </button>
                      </td>
                      <td>{row.updated_at}</td>
                      <td class="actions">
                        <button
                          type="button"
                          class="delete"
                          class:armed={armed === row.id}
                          disabled={affordance.disabled || deleting}
                          title={affordance.disabled ? affordance.title : ''}
                          aria-disabled={affordance['aria-disabled']}
                          onclick={() => doDelete(row.id)}
                        >
                          {armed === row.id ? m['ota.rollout.delete_confirm']() : m['ota.rollout.delete']()}
                        </button>
                        {#if armed === row.id}
                          <button type="button" class="cancel" onclick={() => (armed = null)}>
                            {m['ota.rollout.delete_cancel']()}
                          </button>
                        {/if}
                      </td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            {/if}
          </div>
        {/each}
      </div>
    {/snippet}
  </StateView>
</section>

<style>
  .rollouts {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .upsert {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    gap: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem;
  }
  .target {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    border: none;
    padding: 0;
    margin: 0;
    font-size: 0.85rem;
  }
  .target legend {
    color: var(--fg-muted);
    font-size: 0.8rem;
    padding: 0;
  }
  .radio {
    display: flex;
    align-items: center;
    gap: 0.35rem;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
  }
  .field-label {
    color: var(--fg-muted);
  }
  .field select {
    font: inherit;
  }
  .filter {
    max-width: 24rem;
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
  .groups {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .group {
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
  }
  .group-header {
    width: 100%;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    background: transparent;
    border: none;
    border-radius: 0;
    padding: 0.5rem 0.75rem;
    text-align: left;
  }
  .chevron {
    display: inline-block;
    transition: transform 0.1s linear;
    color: var(--fg-muted);
  }
  .chevron.open {
    transform: rotate(90deg);
  }
  .group-name {
    flex: 1;
    font-weight: 600;
  }
  .group-count {
    color: var(--fg-muted);
    font-size: 0.8rem;
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
    border-top: 1px solid var(--border);
  }
  th {
    color: var(--fg-muted);
    font-weight: 600;
    font-size: 0.8rem;
    border-top: 1px solid var(--border);
  }
  .state-cell {
    display: flex;
    gap: 0.3rem;
  }
  .state-btn.current {
    color: var(--accent);
    border-color: var(--accent);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .delete.armed {
    color: var(--danger);
    border-color: var(--danger);
  }
</style>
