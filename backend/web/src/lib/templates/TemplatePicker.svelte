<script lang="ts">
  import { onMount } from 'svelte'
  import { m } from '../../paraglide/messages.js'
  import { activeLocale, localizedDescription } from '../i18n'
  import { toApiError } from '../api'
  import { session } from '../auth.svelte'
  import { notify } from '../toasts.svelte'
  import { mutationAffordance } from '../readonly'
  import DevicePicker from '../DevicePicker.svelte'
  import type { Device } from '../api/types'
  import { applyTemplate, getTemplate, listTemplates } from './client'
  import {
    applyErrorCode,
    buildApplyParams,
    formValueInit,
    isApplyErrorCode,
    missingRequired,
  } from './form'
  import type { ApplyErrorCode, TemplateDetail, TemplateKind, TemplateSummary } from './types'

  // Template picker panel (A30 W7) — lists the templates whose kind matches the host editor, loads a
  // template's source into that editor (a pure client copy — the operator edits on freely), and applies a
  // template (admin-only) through a form generated from its params schema. Every name/source string
  // renders as a TEXT NODE (Svelte auto-escapes {…}); {@html} is banned (D19.10). The server stays
  // authoritative on /apply (RequireAdmin); the affordance here is cosmetic.
  let { kind, onLoad, devices = [] }: {
    kind: TemplateKind
    onLoad: (source: string) => void
    devices?: Device[]
  } = $props()

  let templates = $state<TemplateSummary[]>([])
  let status = $state<'loading' | 'ready' | 'error'>('loading')
  let loadErr = $state<string | null>(null)
  let busyId = $state<number | null>(null) // the row being loaded/opened

  // apply form (opened for one template at a time)
  let applyFor = $state<TemplateDetail | null>(null)
  let formValues = $state<Record<string, string>>({})
  let applying = $state(false)
  let applyErr = $state<string | null>(null)
  let clientMiss = $state<string[]>([])

  // berry_snippet target
  let targetKind = $state<'one' | 'fleet'>('one')
  let selectedSerial = $state<string | null>(null)
  let typedArm = $state('') // type '*' to arm a fleet broadcast (D23.6)
  // render_fn optional explicit name
  let fnName = $state('')

  const affordance = $derived(mutationAffordance(session.is_admin))
  const fleetCount = $derived(devices.length)

  // Readable message per known param-path 422 (explicit map keeps svelte-check happy — no dynamic m key).
  const APPLY_ERR_MSG: Record<ApplyErrorCode, () => string> = {
    unknown_param: () => m['templates.error.unknown_param'](),
    missing_param: () => m['templates.error.missing_param'](),
    unresolved_placeholder: () => m['templates.error.unresolved_placeholder'](),
    invalid_param: () => m['templates.error.invalid_param'](),
    egress_host_mismatch: () => m['templates.error.egress_host_mismatch'](),
    berry_too_long: () => m['templates.error.berry_too_long'](),
  }

  onMount(reload)

  async function reload(): Promise<void> {
    status = 'loading'
    loadErr = null
    try {
      templates = await listTemplates(kind)
      status = 'ready'
    } catch (e) {
      loadErr = toApiError(e).message
      status = 'error'
    }
  }

  async function loadInto(id: number): Promise<void> {
    busyId = id
    try {
      const t = await getTemplate(id)
      onLoad(t.source)
      notify.success(m['templates.load_success']({ name: t.name }))
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      busyId = null
    }
  }

  async function openApply(id: number): Promise<void> {
    busyId = id
    try {
      const t = await getTemplate(id)
      applyFor = t
      formValues = formValueInit(t.params ?? [])
      applyErr = null
      clientMiss = []
      targetKind = 'one'
      selectedSerial = null
      typedArm = ''
      fnName = ''
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      busyId = null
    }
  }

  function cancelApply(): void {
    applyFor = null
    applyErr = null
    clientMiss = []
  }

  async function submitApply(): Promise<void> {
    const t = applyFor
    if (!t || applying || !session.is_admin) return
    const specs = t.params ?? []
    // Client block: a required field must not reach /apply blank (mirrors the server's missing_param).
    clientMiss = missingRequired(specs, formValues)
    if (clientMiss.length > 0) return

    const body: Record<string, unknown> = { params: buildApplyParams(specs, formValues) }
    if (t.kind === 'berry_snippet') {
      if (targetKind === 'fleet') {
        if (typedArm !== '*') {
          applyErr = m['templates.arm_required']()
          return
        }
        body.target_serials = ['*']
      } else {
        if (!selectedSerial) {
          applyErr = m['templates.target_required']()
          return
        }
        body.target_serials = [selectedSerial]
      }
    } else if (t.kind === 'render_fn' && fnName.trim() !== '') {
      body.fn_name = fnName.trim()
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

<section class="template-picker">
  <header>
    <h3>{m['templates.heading']()}</h3>
    <p class="muted small">{m['templates.subtitle']()}</p>
  </header>

  {#if status === 'loading'}
    <p class="muted" aria-busy="true">{m['templates.loading']()}</p>
  {:else if status === 'error'}
    <div class="tpl-error" role="alert">
      <p>{loadErr}</p>
      <button type="button" onclick={reload}>{m['templates.retry']()}</button>
    </div>
  {:else if templates.length === 0}
    <p class="muted empty">{m['templates.empty']()}</p>
  {:else}
    <ul class="tpl-list">
      {#each templates as t (t.id)}
        <li>
          <div class="tpl-row">
            <span class="tpl-name">{t.name}</span>
            {#if t.builtin}<span class="badge builtin">{m['templates.builtin_badge']()}</span>{/if}
            <span class="muted tpl-ver">v{t.version}</span>
            <div class="tpl-actions">
              <button type="button" disabled={busyId === t.id} onclick={() => loadInto(t.id)}>
                {m['templates.load']()}
              </button>
              <button
                type="button"
                class="primary"
                disabled={affordance.disabled || busyId === t.id}
                title={affordance.disabled ? affordance.title : ''}
                aria-disabled={affordance['aria-disabled']}
                onclick={() => openApply(t.id)}
              >
                {m['templates.apply']()}
              </button>
            </div>
          </div>

          {#if localizedDescription(t.description, activeLocale())}
            <p class="tpl-desc muted small">{localizedDescription(t.description, activeLocale())}</p>
          {/if}

          {#if applyFor && applyFor.id === t.id}
            <form class="tpl-apply" onsubmit={(e) => { e.preventDefault(); void submitApply() }}>
              <p class="apply-title">{m['templates.apply_title']({ name: applyFor.name })}</p>

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
                    <input
                      type="text"
                      bind:value={formValues[p.name]}
                      disabled={affordance.disabled}
                      autocomplete="off"
                      spellcheck="false"
                      placeholder={p.type === 'url' ? 'https://…' : ''}
                    />
                  {/if}
                </label>
              {/each}

              {#if applyFor.kind === 'berry_snippet'}
                <div class="target" role="radiogroup" aria-label="apply target">
                  <span class="lbl">{m['templates.target']()}</span>
                  <label><input type="radio" name="tpl-target" value="one" bind:group={targetKind} /> {m['templates.target_one']()}</label>
                  <label><input type="radio" name="tpl-target" value="fleet" bind:group={targetKind} /> {m['templates.target_fleet']()}</label>
                </div>
                {#if targetKind === 'one'}
                  <DevicePicker {devices} bind:value={selectedSerial} placeholder={m['templates.pick_device']()} />
                {:else}
                  <div class="fleet-arm">
                    <p class="danger" role="status">{m['templates.fleet_warning']({ count: String(fleetCount) })}</p>
                    <label class="arm">
                      {m['templates.arm_label']()}
                      <input type="text" bind:value={typedArm} aria-label="arm fleet broadcast" autocomplete="off" />
                    </label>
                  </div>
                {/if}
              {:else if applyFor.kind === 'render_fn'}
                <label class="param">
                  <span class="param-label">{m['templates.fn_name']()}</span>
                  <input type="text" bind:value={fnName} placeholder={m['templates.fn_name_ph']()} autocomplete="off" spellcheck="false" />
                </label>
              {/if}

              {#if clientMiss.length > 0}
                <p class="inline-err" role="status">{m['templates.param_required']({ names: clientMiss.join(', ') })}</p>
              {/if}
              {#if applyErr}
                <p class="inline-err" role="alert">{applyErr}</p>
              {/if}

              <div class="actions">
                <button
                  type="submit"
                  class="primary"
                  disabled={affordance.disabled || applying}
                  title={affordance.disabled ? affordance.title : ''}
                  aria-disabled={affordance['aria-disabled']}
                >
                  {applying ? m['templates.applying']() : m['templates.submit']()}
                </button>
                <button type="button" onclick={cancelApply}>{m['templates.cancel']()}</button>
              </div>
            </form>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .template-picker {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.75rem;
  }
  .template-picker header {
    margin-bottom: 0.5rem;
  }
  .template-picker h3 {
    margin: 0;
    font-size: 0.95rem;
  }
  .small {
    font-size: 0.8rem;
  }
  .tpl-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .tpl-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .tpl-name {
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.85rem;
  }
  .tpl-ver {
    font-size: 0.75rem;
  }
  .tpl-desc {
    margin: 0.1rem 0 0.2rem;
  }
  .tpl-actions {
    margin-left: auto;
    display: flex;
    gap: 0.35rem;
  }
  .badge.builtin {
    font-size: 0.7rem;
    padding: 0.05rem 0.35rem;
    border: 1px solid var(--border);
    border-radius: 4px;
  }
  .tpl-apply {
    margin: 0.4rem 0 0.2rem;
    padding: 0.5rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .apply-title {
    margin: 0;
    font-weight: 600;
    font-size: 0.85rem;
  }
  .param {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    font-size: 0.8rem;
  }
  .param-label .req {
    color: var(--danger, #c0392b);
  }
  .target {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    font-size: 0.8rem;
    flex-wrap: wrap;
  }
  .fleet-arm .danger {
    color: var(--danger, #c0392b);
    font-size: 0.8rem;
    margin: 0.2rem 0;
  }
  .inline-err {
    color: var(--danger, #c0392b);
    font-size: 0.8rem;
    margin: 0;
  }
  .actions {
    display: flex;
    gap: 0.4rem;
  }
</style>
