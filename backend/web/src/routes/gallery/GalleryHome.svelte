<script lang="ts">
  // Vorlagen gallery — the laien entry area (design/33 §4.5b, W-A33.5b). A two-tile head routes the two
  // laien wishes (own image → /media; use a template → the cards below), then a card grid over the
  // render_fn + berry_snippet templates. The "Verwenden" flow is the whole point: a param form, a MANDATORY
  // device pick, and a confirm whose button stays disabled until a target is chosen (probe a). It sends
  // enable:true + bind:true + target_serials for a render_fn (probe b) — the confirmed click is the
  // deliberate activation act (K15: enabled=false is the frozen default; this area exposes no toggle). A
  // fleet-'*' snippet apply must show the STATIC severing warning (probe c, from summary.ts, never a trace).
  // Every string is a TEXT NODE — no @html directive (S7, AST-gated in `check`).
  import { onMount } from 'svelte'
  import { m } from '../../paraglide/messages.js'
  import { apiFetch, toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import type { CapabilitiesResponse, Manifest } from '../../lib/berry/catalog'
  import { applyTemplate, getTemplate } from '../../lib/templates/client'
  import {
    applyErrorCode,
    formValueInit,
    isApplyErrorCode,
    missingRequired,
  } from '../../lib/templates/form'
  import type {
    ApplyErrorCode,
    TemplateDetail,
    TemplateSummary,
    TemplatesResponse,
  } from '../../lib/templates/types'
  import { buildBerryApply, buildRenderFnApply } from '../../lib/gallery/apply'
  import { severingCalls } from '../../lib/gallery/summary'
  import GalleryCard from '../../lib/gallery/GalleryCard.svelte'

  let templates = $state<TemplateSummary[]>([])
  let status = $state<'loading' | 'ready' | 'error'>('loading')
  let loadErr = $state<string | null>(null)
  let manifest = $state<Manifest | null>(null)
  let deviceList = $state<Device[]>([])

  // apply dialog (one template at a time)
  let applyFor = $state<TemplateDetail | null>(null)
  let formValues = $state<Record<string, string>>({})
  let applying = $state(false)
  let applyErr = $state<string | null>(null)
  let clientMiss = $state<string[]>([])
  // render_fn target
  let rfSerial = $state<string | null>(null)
  let fnName = $state('')
  // berry_snippet target
  let targetKind = $state<'one' | 'fleet'>('one')
  let snSerial = $state<string | null>(null)
  let typedArm = $state('')

  const affordance = $derived(mutationAffordance(session.is_admin))
  const fleetCount = $derived(deviceList.length)

  const APPLY_ERR_MSG: Record<ApplyErrorCode, () => string> = {
    unknown_param: () => m['templates.error.unknown_param'](),
    missing_param: () => m['templates.error.missing_param'](),
    unresolved_placeholder: () => m['templates.error.unresolved_placeholder'](),
    invalid_param: () => m['templates.error.invalid_param'](),
    egress_host_mismatch: () => m['templates.error.egress_host_mismatch'](),
    berry_too_long: () => m['templates.error.berry_too_long'](),
  }

  onMount(() => {
    void reload()
    // Snippet cards + the fleet severing warning need the capability manifest; a failure leaves it null
    // (cards degrade to no summary, the confirm shows no static warning — fail-safe, never a crash).
    void apiFetch<CapabilitiesResponse>('/api/berry/capabilities')
      .then((r) => (manifest = { capabilities: r.capabilities, builtins: r.builtins }))
      .catch(() => (manifest = null))
    void apiFetch<DevicesResponse>('/api/devices')
      .then((r) => (deviceList = r.devices))
      .catch(() => (deviceList = []))
  })

  async function reload(): Promise<void> {
    status = 'loading'
    loadErr = null
    try {
      // No ?kind filter → all kinds; the gallery drops playlist_preset (the Playlist axis owns those, §9).
      const res = await apiFetch<TemplatesResponse>('/api/templates')
      templates = res.templates.filter((t) => t.kind === 'render_fn' || t.kind === 'berry_snippet')
      status = 'ready'
    } catch (e) {
      loadErr = toApiError(e).message
      status = 'error'
    }
  }

  async function openApply(t: TemplateSummary): Promise<void> {
    try {
      const detail = await getTemplate(t.id)
      applyFor = detail
      formValues = formValueInit(detail.params ?? [])
      applyErr = null
      clientMiss = []
      rfSerial = null
      fnName = ''
      targetKind = 'one'
      snSerial = null
      typedArm = ''
    } catch (e) {
      notify.error(toApiError(e))
    }
  }

  function cancelApply(): void {
    applyFor = null
    applyErr = null
    clientMiss = []
  }

  // The severing calls a snippet apply would run — STATIC (summary.ts), branch-independent (S8). Empty for
  // render_fn or when the manifest is unavailable.
  const severing = $derived(
    applyFor && applyFor.kind === 'berry_snippet' && manifest
      ? severingCalls(applyFor.source, manifest)
      : [],
  )

  // The apply body, or null when the mandatory target is not yet chosen — the confirm button's gate (a).
  const applyBodyOrNull = $derived.by<Record<string, unknown> | null>(() => {
    const t = applyFor
    if (!t) return null
    const specs = t.params ?? []
    if (t.kind === 'render_fn') {
      return buildRenderFnApply(specs, formValues, { serial: rfSerial, fnName })
    }
    return buildBerryApply(specs, formValues, {
      mode: targetKind,
      serial: snSerial,
      armed: typedArm === '*',
    })
  })

  const canConfirm = $derived(applyBodyOrNull !== null && !affordance.disabled && !applying)

  async function submitApply(): Promise<void> {
    const t = applyFor
    if (!t || applying || !session.is_admin) return
    const specs = t.params ?? []
    clientMiss = missingRequired(specs, formValues)
    if (clientMiss.length > 0) return
    const body = applyBodyOrNull
    if (!body) {
      applyErr = m['gallery.apply.device_required']()
      return
    }
    applying = true
    applyErr = null
    try {
      await applyTemplate(t.id, body)
      notify.success(m['templates.apply_success']({ name: t.name }))
      applyFor = null
    } catch (e) {
      const err = toApiError(e)
      const code = applyErrorCode(err)
      applyErr = isApplyErrorCode(code) ? APPLY_ERR_MSG[code]() : err.message || m['templates.error.generic']()
    } finally {
      applying = false
    }
  }
</script>

<section class="gallery">
  <header>
    <h1>{m['gallery.title']()}</h1>
    <p class="muted">{m['gallery.intro']()}</p>
  </header>

  <div class="tiles">
    <a class="tile" href="/media">
      <span class="tile-title">{m['gallery.tile_upload_title']()}</span>
      <span class="tile-desc muted">{m['gallery.tile_upload_desc']()}</span>
    </a>
    <a class="tile" href="#gallery-cards">
      <span class="tile-title">{m['gallery.tile_templates_title']()}</span>
      <span class="tile-desc muted">{m['gallery.tile_templates_desc']()}</span>
    </a>
  </div>

  {#if applyFor}
    <div class="apply" role="dialog" aria-modal="true" aria-label={m['gallery.apply.title']({ name: applyFor.name })}>
      <h2>{m['gallery.apply.title']({ name: applyFor.name })}</h2>

      <form onsubmit={(e) => { e.preventDefault(); void submitApply() }}>
        {#each applyFor.params ?? [] as p (p.name)}
          <label class="param">
            <span class="param-label">
              {p.label || p.name}{#if p.required}<span class="req" aria-hidden="true"> *</span>{/if}
            </span>
            {#if p.type === 'enum'}
              <select bind:value={formValues[p.name]} disabled={affordance.disabled}>
                {#each p.options ?? [] as opt (opt)}<option value={opt}>{opt}</option>{/each}
              </select>
            {:else if p.type === 'number'}
              <input type="number" bind:value={formValues[p.name]} disabled={affordance.disabled} />
            {:else}
              <input type="text" bind:value={formValues[p.name]} disabled={affordance.disabled} autocomplete="off" spellcheck="false" placeholder={p.type === 'url' ? 'https://…' : ''} />
            {/if}
          </label>
        {/each}

        <fieldset class="target">
          <legend>{m['gallery.apply.target_heading']()}</legend>
          {#if applyFor.kind === 'render_fn'}
            <DevicePicker devices={deviceList} bind:value={rfSerial} placeholder={m['templates.pick_device']()} />
            <label class="param">
              <span class="param-label">{m['templates.fn_name']()}</span>
              <input type="text" bind:value={fnName} placeholder={m['templates.fn_name_ph']()} autocomplete="off" spellcheck="false" />
            </label>
            <p class="note muted">{m['gallery.apply.enable_note']()}</p>
          {:else}
            <div class="modes" role="radiogroup" aria-label={m['templates.apply_target_aria']()}>
              <label><input type="radio" name="gal-target" value="one" bind:group={targetKind} /> {m['templates.target_one']()}</label>
              <label><input type="radio" name="gal-target" value="fleet" bind:group={targetKind} /> {m['templates.target_fleet']()}</label>
            </div>
            {#if targetKind === 'one'}
              <DevicePicker devices={deviceList} bind:value={snSerial} placeholder={m['templates.pick_device']()} />
            {:else}
              <p class="danger" role="status">{m['templates.fleet_warning']({ count: String(fleetCount) })}</p>
              <label class="arm">
                {m['templates.arm_label']()}
                <input type="text" bind:value={typedArm} aria-label={m['templates.arm_broadcast_aria']()} autocomplete="off" />
              </label>
            {/if}
            {#if severing.length > 0}
              <div class="severing" role="alert">
                <p class="sev-head">{m['gallery.apply.severing_heading']()}</p>
                <ul>
                  {#each severing as name (name)}
                    <li>{m['gallery.apply.severing_item']({ name })}</li>
                  {/each}
                </ul>
                <p class="sev-note muted">{m['gallery.apply.severing_note']()}</p>
              </div>
            {/if}
          {/if}
        </fieldset>

        {#if clientMiss.length > 0}
          <p class="inline-err" role="status">{m['templates.param_required']({ names: clientMiss.join(', ') })}</p>
        {/if}
        {#if applyErr}
          <p class="inline-err" role="alert">{applyErr}</p>
        {/if}

        <div class="actions">
          <button type="submit" class="primary" disabled={!canConfirm} title={affordance.disabled ? affordance.title : ''} aria-disabled={affordance['aria-disabled']}>
            {applying ? m['templates.applying']() : m['gallery.apply.confirm']()}
          </button>
          <button type="button" onclick={cancelApply}>{m['templates.cancel']()}</button>
        </div>
      </form>
    </div>
  {/if}

  <div id="gallery-cards" class="cards-region">
    {#if status === 'loading'}
      <p class="muted" aria-busy="true">{m['gallery.loading']()}</p>
    {:else if status === 'error'}
      <div class="load-error" role="alert">
        <p>{loadErr}</p>
        <button type="button" onclick={reload}>{m['gallery.retry']()}</button>
      </div>
    {:else if templates.length === 0}
      <div class="empty">
        <p class="empty-title">{m['gallery.empty_title']()}</p>
        <p class="muted">{m['gallery.empty_hint']()}</p>
        <a class="btn" href="/media">{m['gallery.empty_cta_upload']()}</a>
      </div>
    {:else}
      <div class="grid">
        {#each templates as t (t.id)}
          <GalleryCard template={t} {manifest} onUse={openApply} />
        {/each}
      </div>
    {/if}
  </div>
</section>

<style>
  .gallery {
    display: flex;
    flex-direction: column;
    gap: 1rem;
    max-width: 60rem;
  }
  header h1 {
    margin: 0 0 0.2rem;
  }
  .muted {
    color: var(--fg-muted);
  }
  .tiles {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(15rem, 1fr));
    gap: 0.75rem;
  }
  .tile {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    text-decoration: none;
    color: var(--fg);
    background: var(--bg-elev, var(--bg));
  }
  .tile:hover {
    border-color: var(--accent, #7aa2f7);
  }
  .tile-title {
    font-weight: 600;
  }
  .tile-desc {
    font-size: 0.85rem;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(15rem, 1fr));
    gap: 0.9rem;
  }
  .apply {
    border: 1px solid var(--accent, #7aa2f7);
    border-radius: 8px;
    padding: 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }
  .apply h2 {
    margin: 0;
    font-size: 1rem;
  }
  form {
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .param {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    font-size: 0.85rem;
  }
  .param-label .req {
    color: var(--danger, #c0392b);
  }
  .target {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.6rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .target legend {
    font-size: 0.85rem;
    font-weight: 600;
    padding: 0 0.3rem;
  }
  .modes {
    display: flex;
    gap: 1rem;
    font-size: 0.85rem;
    flex-wrap: wrap;
  }
  .arm {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.82rem;
  }
  .danger {
    color: var(--danger, #c0392b);
    font-size: 0.82rem;
    margin: 0;
  }
  .note {
    font-size: 0.8rem;
    margin: 0;
  }
  .severing {
    border-left: 3px solid var(--warn, #d08770);
    background: color-mix(in srgb, var(--warn, #d08770) 8%, transparent);
    border-radius: 0 6px 6px 0;
    padding: 0.4rem 0.6rem;
    font-size: 0.82rem;
  }
  .sev-head {
    margin: 0 0 0.2rem;
    font-weight: 600;
    color: var(--warn, #d08770);
  }
  .severing ul {
    margin: 0;
    padding-left: 1.1rem;
  }
  .sev-note {
    margin: 0.3rem 0 0;
    font-size: 0.76rem;
  }
  .inline-err {
    color: var(--danger, #c0392b);
    font-size: 0.82rem;
    margin: 0;
  }
  .actions {
    display: flex;
    gap: 0.5rem;
  }
  button,
  .btn {
    font-size: 0.85rem;
    padding: 0.35rem 0.7rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: transparent;
    color: var(--fg);
    cursor: pointer;
    text-decoration: none;
  }
  .primary {
    background: var(--accent, #7aa2f7);
    color: var(--accent-fg, #10131c);
    border-color: transparent;
    font-weight: 600;
  }
  .primary:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .empty {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    align-items: flex-start;
    padding: 1.5rem;
    border: 1px dashed var(--border);
    border-radius: 8px;
  }
  .empty-title {
    margin: 0;
    font-weight: 600;
  }
  .load-error {
    color: var(--danger, #c0392b);
  }
</style>
