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
  //
  // W5 + E4 (§4.4 Resolve-Vorschau, §8-OQ4 Abweichung "auch wechseln"): a
  // DevicePicker + "Auflösen" resolves GET /api/resolve/{serial} (the SAME
  // resolver ingest serves on /pp, D20.2/T12) into {version, source}. E4 adds
  // the resolved device's CURRENT channel (read from the already-fetched
  // deviceList, §6 reuse) plus an admin-gated channel-<select> + "Wechseln"
  // that PATCHes /api/devices/{serial} (devices.go:88-117) — the same
  // mutation Fleet is expected to carry too (design intentionally duplicates
  // it, §8-OQ4/§9 merge-point). source='none' (fail-open, read.go:18-24) is
  // NOT an error — it renders through the identical ready-path as every other
  // source (N2, ota.test.ts).
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
  import {
    deviceChannelErrorText,
    isRolloutGone,
    isValidRolloutSerial,
    resolveSourceLabel,
    rolloutErrorText,
  } from '../../lib/ota/rollouts'
  import type {
    ChannelsResponse,
    FirmwareResponse,
    ResolveResponse,
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

  // §6: device roster feeds all three DevicePicker surfaces below (upsert
  // target + list filter + W5 resolve-vorschau) — same raw-apiFetch-into-array
  // pattern as MediaHome's panel picker (no Resource wrapper: a failed fetch
  // just leaves every picker empty).
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

  // --- Resolve-Vorschau (§4.4/§7-W5, GET /api/resolve/{serial}) + E4-Erweiterung
  // (Device-Channel-Wechsel, PATCH /api/devices/{serial}, devices.go:88-117) ---
  let resolveSerial = $state<string | null>(null)
  let resolving = $state(false)
  let resolved = $state<ResolveResponse | null>(null)
  let resolveErr = $state<string | null>(null)

  // E4: the CURRENT channel of the resolved device. Cheapest correct source —
  // `deviceList` (fetched once via GET /api/devices onMount, §6, reused here)
  // already carries `channel` per row (api/types.ts Device.channel); a second
  // request would be redundant. `GET /api/resolve/{serial}` itself does NOT
  // carry the device's channel — `Resolved = {version, source}` (types.ts:40-43)
  // is the RESOLVER OUTPUT (which version a device gets), not device identity.
  const resolvedDevice = $derived(deviceList.find((d) => d.serial === resolveSerial) ?? null)
  let pickedChannel = $state('')
  let changingChannel = $state(false)

  async function resolveDevice(serial: string): Promise<void> {
    resolving = true
    resolveErr = null
    try {
      resolved = await apiFetch<ResolveResponse>(`/api/resolve/${serial}`)
      // §7-W5 N2: source='none' (fail-open, read.go:18-24 — unknown serial or
      // simply no target) flows through this SAME success assignment as every
      // other source; nothing here branches on it as an error. It renders via
      // resolveSourceLabel in the template below, same as 'serial'/'fleet'/
      // 'channel-default'.
      pickedChannel = resolvedDevice?.channel ?? ''
    } catch (err) {
      resolved = null
      resolveErr = otaErrorText(err)
    } finally {
      resolving = false
    }
  }

  function onResolveClick(): void {
    if (resolveSerial === null) return
    void resolveDevice(resolveSerial)
  }

  // E4 "auch wechseln" (Abweichung von der read-only-Empfehlung §8-OQ4): admin-
  // gated Channel-Wechsel direkt aus der Vorschau. Nach Erfolg wird der Resolve
  // NEU ausgeführt, weil der Wechsel den Resolver-Input (devices.channel,
  // read.go:11-14) ändert — die alte {version, source}-Anzeige wäre sonst
  // stale. `rollouts.reload()` ist bewusst NICHT dabei: rollout_targets-Rows
  // tragen keinen Device-Channel-Bezug (RolloutRow, types.ts:24-31), ein
  // Channel-Move ändert an der Rollout-LISTE nichts.
  async function submitChannelChange(): Promise<void> {
    if (!session.is_admin || resolveSerial === null || pickedChannel === '' || changingChannel) return // Handler-Guard (§5 B1)
    changingChannel = true
    try {
      const res = await apiFetch<{ success: true; device: Device }>(`/api/devices/${resolveSerial}`, {
        method: 'PATCH',
        body: JSON.stringify({ channel: pickedChannel }),
      })
      // keep the roster in sync so the picker/current-channel display reflect
      // the move immediately, without a full GET /api/devices refetch.
      deviceList = deviceList.map((d) => (d.serial === res.device.serial ? res.device : d))
      notify.success(m['ota.resolve.channel_changed']({ serial: res.device.serial, channel: res.device.channel }))
      await resolveDevice(resolveSerial)
    } catch (err) {
      notify.error(deviceChannelErrorText(err)) // not_found ("Gerät") / unknown_channel — NIE otaErrorText's Channel-not_found
    } finally {
      changingChannel = false
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

  <section class="resolve" aria-label={m['ota.resolve.heading']()}>
    <h2>{m['ota.resolve.heading']()}</h2>

    <DevicePicker devices={deviceList} bind:value={resolveSerial} placeholder={m['ota.resolve.pick']()} />

    <button type="button" onclick={onResolveClick} disabled={resolveSerial === null || resolving}>
      {m['ota.resolve.resolve']()}
    </button>

    {#if resolveErr}
      <p class="resolve-error" role="alert">{resolveErr}</p>
    {/if}

    {#if resolved}
      <div class="resolve-result">
        <p>
          <span class="field-label">{m['ota.resolve.version']()}:</span>
          {resolved.resolved.version || '—'}
          <span class="source-tag">({resolveSourceLabel(resolved.resolved.source)})</span>
        </p>

        {#if resolvedDevice}
          <p>
            <span class="field-label">{m['ota.rollout.channel']()}:</span>
            {resolvedDevice.channel}
          </p>
          <div class="channel-move">
            <select
              bind:value={pickedChannel}
              disabled={affordance.disabled || changingChannel}
              aria-label={m['ota.rollout.channel']()}
            >
              {#each channels.data?.channels ?? [] as ch (ch.name)}
                <option value={ch.name}>{ch.name}</option>
              {/each}
            </select>
            <button
              type="button"
              onclick={submitChannelChange}
              disabled={affordance.disabled ||
                changingChannel ||
                pickedChannel === '' ||
                pickedChannel === resolvedDevice.channel}
              title={affordance.disabled ? affordance.title : ''}
              aria-disabled={affordance['aria-disabled']}
            >
              {m['ota.resolve.change']()}
            </button>
          </div>
        {/if}
      </div>
    {/if}
  </section>
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
  .resolve {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    border-top: 1px solid var(--border);
    padding-top: 0.75rem;
  }
  .resolve-error {
    color: var(--danger);
    font-size: 0.85rem;
    margin: 0;
  }
  .resolve-result {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.6rem 0.75rem;
    font-size: 0.875rem;
  }
  .resolve-result p {
    margin: 0;
  }
  .field-label {
    color: var(--fg-muted);
  }
  .source-tag {
    color: var(--fg-muted);
  }
  .channel-move {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .channel-move select {
    font: inherit;
  }
</style>
