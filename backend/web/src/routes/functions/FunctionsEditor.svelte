<script lang="ts">
  import { onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import { apiBinary } from '../../lib/api-binary'
  import StateView from '../../lib/StateView.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { loadDraft, saveDraft, clearDraft } from '../../lib/draft'
  import { createFaasEditor, type FaasEditorHandle } from '../../lib/faas/editor'
  import { CAP_ENTRIES, CTX_ENTRIES, RETURN_ENTRIES, TEMPLATE } from '../../lib/faas/catalog'
  import {
    DITHER_OPTIONS,
    RENDER_MODES,
    emptyTriggerFields,
    parseTriggerFields,
    buildTriggerConfig,
    parseEgress,
    formatEgress,
    webhookURL,
    validCron,
    type TriggerFields,
  } from '../../lib/faas/config'
  import { TRIGGER_TYPES, newFunctionError, blastRadiusLabel, type NewFunctionDraft } from '../../lib/faas/functions'
  import FramePreview from '../../lib/media/FramePreview.svelte'
  import TemplatePicker from '../../lib/templates/TemplatePicker.svelte'
  import { m } from '../../paraglide/messages.js'
  import { parseTestFrame, buildTestRunBody, type TestRunResult } from '../../lib/faas/testrun'
  import type {
    TriggerType,
    FunctionsResponse,
    FunctionResponse,
    CreateFunctionResponse,
    SecretsResponse,
    ConfigResponse,
  } from '../../lib/faas/types'
  import type { Device, DevicesResponse } from '../../lib/api/types'

  // FaaS function editor (Design 25, W2) — the CM6 code editor + capability completion + the config forms
  // (trigger / egress / secrets / dither / enabled, Policy=Data over the faas_functions row) + the
  // webhook-token surface. AUTHOR-ONLY: no test-run / execution ships here (that is W3, admin-gated + the
  // first RCE surface). Every function source, config value, secret NAME, egress host and serial renders
  // as a TEXT NODE (Svelte auto-escapes {…}); {@html} is banned (D19.10). The server stays authoritative
  // on every mutation via requireAdmin (D25.11); the read-only affordance is cosmetic — a non-admin sees a
  // read-only editor + no Save/Enable/Delete/rotate/secret-edit.

  const functions = new Resource<FunctionsResponse>(() => apiFetch<FunctionsResponse>('/api/functions'))
  void functions.load()

  // non-secret SPA config (webhook base URL, D22.13) — loaded once, best-effort (a failure just hides the
  // webhook URL). GET /api/config is auth-gated (any key).
  let webhookBase = $state('')
  void apiFetch<ConfigResponse>('/api/config')
    .then((c) => (webhookBase = c.webhook_base_url))
    .catch(() => {})

  // secret NAMES for the binding picker (Doc 18 metadata, D25.7). GET /api/secrets is admin-only, so this
  // only loads for an admin (a read-only operator sees the bound names as read-only chips, never a value).
  let secretNames = $state<string[]>([])
  if (session.is_admin) {
    void apiFetch<SecretsResponse>('/api/secrets')
      .then((r) => (secretNames = r.secrets.map((s) => s.name)))
      .catch(() => {})
  }

  // device list for the binding picker (D19.12/K13). Loaded once, best-effort; a read-only operator can
  // still see the roster (GET /api/devices is auth-gated), but the bind/unbind affordances are admin-only.
  let deviceList = $state<Device[]>([])
  void apiFetch<DevicesResponse>('/api/devices')
    .then((r) => (deviceList = r.devices))
    .catch(() => {})

  // ---- selection + loaded detail ----
  let selectedId = $state<number | null>(null)
  let detail = $state<FunctionResponse | null>(null)
  let detailStatus = $state<'idle' | 'loading' | 'error'>('idle')
  let detailError = $state<string | null>(null)

  // ---- editable state (mirrors the loaded function; the editor drives `source`) ----
  let source = $state('')
  let boundSecrets = $state<string[]>([])
  let trigFields = $state<TriggerFields>(emptyTriggerFields())
  let egressText = $state('')
  // baseline snapshot to derive the dirty flag (source + config vs what was loaded).
  let baseline = $state<{ source: string; trig: string; egress: string; secrets: string } | null>(null)

  // ---- create form ----
  let newName = $state('')
  let newTrigger = $state<TriggerType>('render')
  let creating = $state(false)

  // webhook token shown ONCE on create/rotate (D24.13).
  let mintedToken = $state<{ name: string; token: string } | null>(null)
  let saving = $state(false)
  let mutating = $state(false)

  const affordance = $derived(mutationAffordance(session.is_admin))
  const createDraft = $derived<NewFunctionDraft>({ name: newName, source: TEMPLATE, triggerType: newTrigger })
  const createError = $derived(newFunctionError(createDraft))

  const dirty = $derived(
    baseline !== null &&
      detail !== null &&
      (source !== baseline.source ||
        JSON.stringify(buildTriggerConfig(detail.function.trigger_type, trigFields)) !== baseline.trig ||
        JSON.stringify(parseEgress(egressText)) !== baseline.egress ||
        JSON.stringify([...boundSecrets].sort()) !== baseline.secrets),
  )
  const cronOk = $derived(detail?.function.trigger_type !== 'schedule' || validCron(trigFields.cron))
  const draftScope = $derived(selectedId === null ? '' : `faas:${selectedId}`)

  // ---- CM6 editor: one instance, reconciled whenever the mount node changes identity (select / deselect /
  // remount). setDoc re-seeds it on a selection change; setReadOnly tracks a live admin demotion. ----
  let editorEl = $state<HTMLDivElement | null>(null)
  let handle: FaasEditorHandle | null = null
  let mountedNode: HTMLElement | null = null
  // Editor TypeScript language service: 'off' until the editor mounts, then idle-loads (mirrors the WASM
  // idle-load of the simulator, W-A33.3). 'unavailable' is the named fail-open state — the editor stays
  // fully usable regardless.
  let tsStatus = $state<'off' | 'loading' | 'ready' | 'unavailable'>('off')

  $effect(() => {
    const el = editorEl
    if (el === mountedNode) return
    handle?.destroy()
    handle = null
    mountedNode = el
    if (el) {
      const h = createFaasEditor({
        parent: el,
        doc: source,
        getBound: () => boundSecrets,
        readOnly: !session.is_admin,
        onChange: (d) => {
          source = d
          if (selectedId !== null) saveDraft(`faas:${selectedId}`, d)
        },
      })
      handle = h
      // Idle-load the language service after first paint so the editor opens instantly; a failure just
      // leaves it 'unavailable' (fail-open).
      tsStatus = 'loading'
      const idle =
        typeof requestIdleCallback === 'function'
          ? requestIdleCallback
          : (cb: () => void) => setTimeout(cb, 200)
      idle(() => {
        h.enableTypeService().then(
          (s) => (tsStatus = s),
          () => (tsStatus = 'unavailable'),
        )
      })
    }
  })
  // live admin demotion → the editor goes read-only without a reload (D25.11).
  $effect(() => {
    handle?.setReadOnly(!session.is_admin)
  })
  onDestroy(() => handle?.destroy())

  async function loadDetail(id: number): Promise<void> {
    selectedId = id
    detailStatus = 'loading'
    detailError = null
    try {
      const res = await apiFetch<FunctionResponse>(`/api/functions/${id}`)
      detail = res
      const fn = res.function
      const draft = loadDraft(`faas:${id}`)
      const initial = draft ?? fn.source
      source = initial
      boundSecrets = [...fn.secret_bindings]
      trigFields = parseTriggerFields(fn.trigger_config)
      egressText = formatEgress(fn.egress_allow)
      baseline = snapshot(fn.trigger_type)
      handle?.setDoc(initial)
      // reset the test-run surface for the new selection; default the context serial to a bound device.
      testResult = null
      testError = null
      testSerial = res.bound_serials[0] ?? ''
      testPayload = ''
      detailStatus = 'idle'
    } catch (e) {
      detail = null
      detailError = toApiError(e).message
      detailStatus = 'error'
    }
  }

  // the baseline uses the SAVED source (fn.source), not a restored draft — so a restored draft reads dirty.
  function snapshot(triggerType: TriggerType): { source: string; trig: string; egress: string; secrets: string } {
    const fn = detail!.function
    return {
      source: fn.source,
      trig: JSON.stringify(buildTriggerConfig(triggerType, parseTriggerFields(fn.trigger_config))),
      egress: JSON.stringify([...fn.egress_allow]),
      secrets: JSON.stringify([...fn.secret_bindings].sort()),
    }
  }

  // Load a render_fn template's source into the active editor (A30-W7). setDoc dispatches a change, so the
  // onChange listener updates `source` + saves the draft. A load needs a selected function (the editor is
  // only mounted in the detail column); with none selected, hint instead of silently no-op'ing. Apply is
  // independent — it mints a NEW function and does not require a selection.
  function onLoadTemplate(src: string): void {
    if (selectedId === null || !handle) {
      notify.info(m['templates.select_function_first']())
      return
    }
    source = src
    handle.setDoc(src)
    saveDraft(`faas:${selectedId}`, src)
  }

  async function reloadAll(keepSelection: boolean): Promise<void> {
    await functions.load()
    if (keepSelection && selectedId !== null) await loadDetail(selectedId)
  }

  async function doCreate(): Promise<void> {
    if (createError !== null || creating || !session.is_admin) return
    creating = true
    try {
      const res = await apiFetch<CreateFunctionResponse>('/api/functions', {
        method: 'POST',
        body: JSON.stringify({ name: newName.trim(), source: TEMPLATE, trigger_type: newTrigger }),
      })
      notify.success(m['faas.notify.created']({ name: res.name }))
      if (res.webhook_token) mintedToken = { name: res.name, token: res.webhook_token }
      newName = ''
      newTrigger = 'render'
      await functions.load()
      await loadDetail(res.id)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      creating = false
    }
  }

  async function doSave(): Promise<void> {
    if (!detail || saving || !session.is_admin || !dirty || !cronOk) return
    saving = true
    try {
      const fn = detail.function
      await apiFetch(`/api/functions/${fn.id}`, {
        method: 'PUT',
        body: JSON.stringify({
          source,
          trigger_config: buildTriggerConfig(fn.trigger_type, trigFields),
          secret_bindings: boundSecrets,
          egress_allow: parseEgress(egressText),
        }),
      })
      clearDraft(`faas:${fn.id}`)
      const bound = detail.bound_serials.length
      notify.success(
        bound > 0
          ? m['faas.notify.saved_bound']({ name: fn.name, devices: blastRadiusLabel(bound) })
          : m['faas.notify.saved']({ name: fn.name }),
      )
      await reloadAll(true)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      saving = false
    }
  }

  async function toggleEnabled(): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    const fn = detail.function
    try {
      await apiFetch(`/api/functions/${fn.id}`, { method: 'PATCH', body: JSON.stringify({ enabled: !fn.enabled }) })
      notify.success(fn.enabled ? m['faas.notify.toggled_off']({ name: fn.name }) : m['faas.notify.toggled_on']({ name: fn.name }))
      await reloadAll(true)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
    }
  }

  async function rotateToken(): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    const fn = detail.function
    try {
      const res = await apiFetch<{ success: true; webhook_token: string }>(`/api/functions/${fn.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ rotate_token: true }),
      })
      mintedToken = { name: fn.name, token: res.webhook_token }
      notify.success(m['faas.notify.rotated']({ name: fn.name }))
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
    }
  }

  let deleteArmed = $state(false)
  $effect(() => {
    void selectedId
    deleteArmed = false
  })
  async function doDelete(): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    if (!deleteArmed) {
      deleteArmed = true
      return
    }
    mutating = true
    const fn = detail.function
    try {
      await apiFetch(`/api/functions/${fn.id}`, { method: 'DELETE' })
      clearDraft(`faas:${fn.id}`)
      notify.success(m['faas.notify.deleted']({ name: fn.name }))
      selectedId = null
      detail = null
      baseline = null
      await functions.load()
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
      deleteArmed = false
    }
  }

  // ---- device binding (W4, D25.9): which devices render this function. bind = A24 PUT; unbind = W1 DELETE.
  // Binding is a fleet-affecting mutation (the device's next frame changes) → admin-only. ----
  let bindTarget = $state<string | null>(null)

  async function bindDevice(): Promise<void> {
    if (!detail || mutating || !session.is_admin || !bindTarget) return
    mutating = true
    const fn = detail.function
    try {
      await apiFetch(`/api/devices/${bindTarget}/render`, {
        method: 'PUT',
        body: JSON.stringify({ function_id: fn.id }),
      })
      notify.success(m['faas.notify.bound']({ serial: bindTarget, name: fn.name }))
      bindTarget = null
      await reloadAll(true)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
    }
  }

  async function unbindDevice(serial: string): Promise<void> {
    if (!detail || mutating || !session.is_admin) return
    mutating = true
    try {
      await apiFetch(`/api/devices/${serial}/render`, { method: 'DELETE' })
      notify.success(m['faas.notify.unbound']({ serial }))
      await reloadAll(true)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
    }
  }

  // devices available to bind: not already bound to THIS function (avoid a no-op re-bind in the picker).
  const bindableDevices = $derived(
    detail ? deviceList.filter((d) => !detail!.bound_serials.includes(d.serial)) : [],
  )

  function toggleSecret(name: string): void {
    boundSecrets = boundSecrets.includes(name)
      ? boundSecrets.filter((n) => n !== name)
      : [...boundSecrets, name]
  }

  const hookURL = $derived(detail ? webhookURL(webhookBase, detail.function.name) : null)

  // ---- test-run (W3): the first execution surface — admin-only (RCE-equivalent, D25.3/D25.11). It runs
  // the CURRENT editor draft (source + config, the fast loop) against an operator-chosen device context;
  // the supervisor executes it in the prod worker sandbox, secrets STUBBED, side-effect-free (D25.4). The
  // response is a framed binary (meta + packed + raw) parsed client-side and painted onto two canvases.
  let testSerial = $state('')
  let testNow = $state('')
  let testPayload = $state('') // webhook: an operator JSON body → ctx.trigger.payload
  let testing = $state(false)
  let testResult = $state<TestRunResult | null>(null)
  let testError = $state<string | null>(null)

  function payloadValue(): unknown {
    if (!detail || detail.function.trigger_type !== 'webhook' || testPayload.trim() === '') return undefined
    return JSON.parse(testPayload) // caller guards with a try/catch → a parse error is surfaced, no run
  }

  async function runTest(): Promise<void> {
    if (!detail || !session.is_admin || testing) return
    if (testSerial.trim() === '') {
      testError = m['faas.test_serial_required']()
      return
    }
    let payload: unknown
    try {
      payload = payloadValue()
    } catch {
      testError = m['faas.test_payload_invalid']()
      return
    }
    testing = true
    testError = null
    testResult = null
    try {
      const fn = detail.function
      const bodyText = buildTestRunBody(
        {
          source,
          dither: trigFields.dither,
          secret_bindings: boundSecrets,
          egress_allow: parseEgress(egressText),
        },
        { serial: testSerial.trim(), trigger: fn.trigger_type, now: testNow.trim() || undefined, payload },
      )
      // apiBinary (design 29 §4.2) owns the cookie + CSRF injection and the {success:false}
      // envelope parsing this call used to inline: it rides the httpOnly ppk_sid cookie
      // (credentials: 'same-origin'), stamps X-Requested-With: picpak on the POST, tears down
      // the session on a 401 (§5 S6), and returns the raw framed bytes on success.
      const buf = await apiBinary('/api/functions/test-run', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: bodyText,
      })
      testResult = parseTestFrame(buf)
      if (!testResult.meta.ok && testResult.meta.err) {
        notify.warn(m['faas.notify.testrun_err']({ kind: testResult.meta.err.kind }))
      }
    } catch (e) {
      testError = toApiError(e).message
      notify.error(toApiError(e))
    } finally {
      testing = false
    }
  }
</script>

<section class="functions">
  <header>
    <h1>{m['faas.title']()}</h1>
    <p class="muted">
      {m['faas.intro']()}
    </p>
  </header>

  {#if mintedToken}
    <div class="token-banner" role="alert">
      <p>
        {m['faas.token_banner']({ name: mintedToken.name })}
      </p>
      <code class="token">{mintedToken.token}</code>
      <button type="button" class="dismiss" onclick={() => (mintedToken = null)}>{m['faas.dismiss']()}</button>
    </div>
  {/if}

  <StateView resource={functions} isEmpty={() => false} loadingText={m['faas.loading_functions']()}>
    {#snippet ready(data: FunctionsResponse)}
      <div class="grid">
        <div class="list-col">
          <h2>{m['faas.list_heading']()}</h2>
          {#if data.functions.length === 0}
            <p class="muted empty">{m['faas.empty']()}</p>
          {:else}
            <ul class="fn-list" role="listbox">
              {#each data.functions as fn (fn.id)}
                <li>
                  <button
                    type="button"
                    role="option"
                    class="fn-opt"
                    class:selected={fn.id === selectedId}
                    aria-selected={fn.id === selectedId}
                    onclick={() => loadDetail(fn.id)}
                  >
                    <span class="dot" class:on={fn.enabled} title={fn.enabled ? m['faas.status_enabled']() : m['faas.status_disabled']()}></span>
                    <span class="fn-name">{fn.name}</span>
                    <span class="fn-trigger">{fn.trigger_type}</span>
                    <span class="fn-ver">v{fn.version}</span>
                  </button>
                </li>
              {/each}
            </ul>
          {/if}

          <div class="create">
            <h3>{m['faas.create_heading']()}</h3>
            <label>
              {m['faas.name']()}
              <input type="text" bind:value={newName} placeholder={m['faas.name_ph']()} autocomplete="off" spellcheck="false" />
            </label>
            <label>
              {m['faas.trigger']()}
              <select bind:value={newTrigger}>
                {#each TRIGGER_TYPES as tt (tt)}
                  <option value={tt}>{tt}</option>
                {/each}
              </select>
            </label>
            <p class="muted small">{m['faas.create_hint']()}</p>
            {#if newName !== '' && createError !== null}
              <p class="inline-err" role="status">{createError}</p>
            {/if}
            <div class="actions">
              <button
                type="button"
                class="primary"
                disabled={affordance.disabled || createError !== null || creating}
                title={affordance.disabled ? affordance.title : ''}
                aria-disabled={affordance['aria-disabled'] || createError !== null}
                onclick={doCreate}
              >
                {creating ? m['faas.creating']() : m['faas.create']()}
              </button>
              {#if newTrigger === 'webhook'}
                <span class="muted hint">{m['faas.webhook_mint_hint']()}</span>
              {/if}
            </div>
          </div>

          <TemplatePicker kind="render_fn" devices={deviceList} onLoad={onLoadTemplate} />
        </div>

        <div class="detail-col">
          {#if detailStatus === 'loading'}
            <p class="muted" aria-busy="true">{m['faas.loading_detail']()}</p>
          {:else if detailStatus === 'error'}
            <div class="detail-error" role="alert">
              <p>{detailError}</p>
              {#if selectedId !== null}
                <button type="button" onclick={() => selectedId !== null && loadDetail(selectedId)}>{m['faas.retry']()}</button>
              {/if}
            </div>
          {:else if detail}
            {@const fn = detail.function}
            <div class="detail">
              <div class="detail-head">
                <h2>{fn.name}</h2>
                <span class="badge">{fn.trigger_type}</span>
                <span class="badge" class:on={fn.enabled}>{fn.enabled ? m['faas.status_enabled']() : m['faas.status_disabled']()}</span>
                <span class="muted">v{fn.version}</span>
                {#if dirty}<span class="badge dirty">{m['faas.unsaved']()}</span>{/if}
              </div>

              {#if !session.is_admin}
                <p class="muted small">{m['faas.readonly_hint']()}</p>
              {/if}

              <div class="editor-wrap">
                <div class="editor" bind:this={editorEl}></div>
                {#if tsStatus !== 'off'}
                  <p class="muted small ts-status" class:ts-ready={tsStatus === 'ready'}>
                    {#if tsStatus === 'loading'}{m['faas.typeservice.loading']()}
                    {:else if tsStatus === 'ready'}{m['faas.typeservice.ready']()}
                    {:else}{m['faas.typeservice.unavailable']()}{/if}
                  </p>
                {/if}
              </div>

              <div class="config">
                <h3>{m['faas.config_trigger']({ type: fn.trigger_type })}</h3>
                {#if fn.trigger_type === 'render'}
                  <div class="row">
                    <label>
                      {m['faas.mode']()}
                      <select bind:value={trigFields.mode} disabled={affordance.disabled}>
                        <option value="">{m['faas.mode_sync']()}</option>
                        {#each RENDER_MODES as mode (mode)}<option value={mode}>{mode}</option>{/each}
                      </select>
                    </label>
                    <label>
                      {m['faas.ttl_s']()}
                      <input type="number" min="0" bind:value={trigFields.ttlS} disabled={affordance.disabled} />
                    </label>
                    <label>
                      {m['faas.interval_s']()}
                      <input type="number" min="0" bind:value={trigFields.intervalS} disabled={affordance.disabled} />
                    </label>
                  </div>
                {:else if fn.trigger_type === 'schedule'}
                  <div class="row">
                    <label class="grow">
                      {m['faas.cron']()}
                      <input
                        type="text"
                        bind:value={trigFields.cron}
                        placeholder="m h dom mon dow"
                        disabled={affordance.disabled}
                        spellcheck="false"
                      />
                    </label>
                    <label>
                      {m['faas.interval_s']()}
                      <input type="number" min="0" bind:value={trigFields.intervalS} disabled={affordance.disabled} />
                    </label>
                  </div>
                  {#if !cronOk}<p class="inline-err">{m['faas.cron_invalid']()}</p>{/if}
                  <p class="muted small">{m['faas.cron_hint']()}</p>
                {:else}
                  <div class="webhook">
                    {#if hookURL}
                      <label class="grow">
                        {m['faas.inbound_url']()}
                        <input type="text" class="mono" readonly value={hookURL} />
                      </label>
                    {:else}
                      <p class="muted small">{m['faas.webhook_hidden']()}</p>
                    {/if}
                    <button
                      type="button"
                      disabled={affordance.disabled || mutating}
                      title={affordance.disabled ? affordance.title : ''}
                      onclick={rotateToken}
                    >
                      {m['faas.rotate_token']()}
                    </button>
                  </div>
                {/if}

                <div class="row wrap">
                  <label>
                    {m['faas.dither']()}
                    <select bind:value={trigFields.dither} disabled={affordance.disabled}>
                      {#each DITHER_OPTIONS as d (d.value)}<option value={d.value}>{d.label}</option>{/each}
                    </select>
                  </label>
                  <div class="enabled-toggle">
                    <span class="flabel">{m['faas.field_enabled']()}</span>
                    <button
                      type="button"
                      class:on={fn.enabled}
                      disabled={affordance.disabled || mutating}
                      title={affordance.disabled ? affordance.title : ''}
                      onclick={toggleEnabled}
                    >
                      {fn.enabled ? m['faas.on']() : m['faas.off']()}
                    </button>
                  </div>
                </div>

                <div class="field">
                  <span class="flabel">{m['faas.egress_label']()}</span>
                  <textarea
                    bind:value={egressText}
                    rows="2"
                    placeholder="host.example:8123"
                    disabled={affordance.disabled}
                    spellcheck="false"
                  ></textarea>
                </div>

                <div class="field">
                  <span class="flabel">{m['faas.secrets_label']()}</span>
                  {#if session.is_admin}
                    {#if secretNames.length === 0}
                      <p class="muted small">{m['faas.secrets_empty']()}</p>
                    {:else}
                      <ul class="secret-picker">
                        {#each secretNames as name (name)}
                          <li>
                            <label>
                              <input
                                type="checkbox"
                                checked={boundSecrets.includes(name)}
                                onchange={() => toggleSecret(name)}
                              />
                              <span class="mono">{name}</span>
                            </label>
                          </li>
                        {/each}
                      </ul>
                    {/if}
                  {:else if boundSecrets.length > 0}
                    <ul class="chips">
                      {#each boundSecrets as name (name)}<li class="chip mono">{name}</li>{/each}
                    </ul>
                  {:else}
                    <p class="muted small">{m['faas.secrets_none']()}</p>
                  {/if}
                </div>

                {#if dirty && detail.bound_serials.length > 0}
                  <p class="blast-warn" role="status">
                    {m['faas.blast_warn']({ devices: blastRadiusLabel(detail.bound_serials.length) })}
                  </p>
                {/if}
                <div class="save-row">
                  <button
                    type="button"
                    class="primary"
                    disabled={affordance.disabled || saving || !dirty || !cronOk}
                    title={affordance.disabled ? affordance.title : ''}
                    aria-disabled={affordance['aria-disabled'] || !dirty}
                    onclick={doSave}
                  >
                    {saving ? m['faas.saving']() : m['faas.save']()}
                  </button>
                  <button
                    type="button"
                    class="danger"
                    disabled={affordance.disabled || mutating}
                    title={affordance.disabled ? affordance.title : ''}
                    onclick={doDelete}
                  >
                    {deleteArmed ? m['faas.confirm_delete']({ devices: blastRadiusLabel(detail.bound_serials.length) }) : m['faas.delete']()}
                  </button>
                  {#if deleteArmed}<button type="button" class="link" onclick={() => (deleteArmed = false)}>{m['faas.cancel']()}</button>{/if}
                  <span class="muted small blast">{m['faas.bound']({ devices: blastRadiusLabel(detail.bound_serials.length) })}</span>
                </div>
              </div>

              <div class="bindings">
                <h3>{m['faas.bindings_heading']({ devices: blastRadiusLabel(detail.bound_serials.length) })}</h3>
                {#if detail.bound_serials.length > 0}
                  <ul class="chips">
                    {#each detail.bound_serials as s (s)}
                      <li class="chip mono bind-chip">
                        <span>{s}</span>
                        {#if session.is_admin}
                          <button type="button" class="chip-x" title={m['faas.unbind_title']({ serial: s })} disabled={mutating} onclick={() => unbindDevice(s)}>✕</button>
                        {/if}
                      </li>
                    {/each}
                  </ul>
                  <p class="muted small">{m['faas.bindings_note']()}</p>
                {:else}
                  <p class="muted small">{m['faas.not_bound']()}</p>
                {/if}
                {#if session.is_admin}
                  <div class="bind-row">
                    <DevicePicker devices={bindableDevices} bind:value={bindTarget} placeholder={m['faas.bind_ph']()} />
                    <button type="button" disabled={mutating || !bindTarget} onclick={bindDevice}>{m['faas.bind']()}</button>
                  </div>
                {/if}
              </div>

              {#if session.is_admin}
                <div class="testrun">
                  <h3>{m['faas.testrun_heading']()}</h3>
                  <p class="muted small">
                    {m['faas.testrun_note']()}
                  </p>
                  <div class="row wrap">
                    <label class="grow">
                      {m['faas.test_serial']()}
                      <input type="text" bind:value={testSerial} placeholder={m['faas.test_serial_ph']()} spellcheck="false" />
                    </label>
                    <label>
                      {m['faas.test_now']()}
                      <input type="text" bind:value={testNow} placeholder="RFC3339" spellcheck="false" />
                    </label>
                  </div>
                  {#if fn.trigger_type === 'webhook'}
                    <label class="field">
                      {m['faas.test_payload']()}
                      <textarea bind:value={testPayload} rows="2" placeholder={'{ "key": "value" }'} spellcheck="false"></textarea>
                    </label>
                  {/if}
                  <div class="actions">
                    <button type="button" class="primary" disabled={testing} onclick={runTest}>
                      {testing ? m['faas.running']() : m['faas.run_test']()}
                    </button>
                    {#if testError}<span class="inline-err">{testError}</span>{/if}
                  </div>

                  {#if testResult}
                    {@const meta = testResult.meta}
                    <div class="result">
                      <div class="result-head">
                        <span class="badge" class:on={meta.ok} class:err={!meta.ok}>{meta.status}</span>
                        <span class="muted small">{m['faas.result_wake']({ wake: meta.wake, dither: meta.dither })}</span>
                      </div>
                      {#if meta.err}
                        <p class="inline-err">{m['faas.result_error']({ kind: meta.err.kind, msg: meta.err.msg })}</p>
                      {/if}
                      <FramePreview
                        packed={testResult.packed}
                        raw={meta.raw_fmt === 'rgb' ? testResult.raw : null}
                        alt={m['faas.panel_alt']()}
                        rawAlt={m['faas.raw_alt']()}
                      />
                      {#if meta.log.length > 0}
                        <div class="log">
                          <span class="flabel">{m['faas.worker_log']()}</span>
                          <ul>
                            {#each meta.log as l, i (i)}
                              <li class="log-{l.lvl}"><span class="lvl">{l.lvl}</span> {l.msg}</li>
                            {/each}
                          </ul>
                        </div>
                      {/if}
                    </div>
                  {/if}
                </div>
              {/if}

              <details class="caps">
                <summary>{m['faas.caps_summary']()}</summary>
                <p class="muted small">
                  {m['faas.caps_note']()}
                </p>
                <h4>{m['faas.caps_cap']()}</h4>
                <ul class="caplist">
                  {#each CAP_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
                <h4>{m['faas.caps_ctx']()}</h4>
                <ul class="caplist">
                  {#each CTX_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
                <h4>{m['faas.caps_return']()}</h4>
                <ul class="caplist">
                  {#each RETURN_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
              </details>
            </div>
          {:else}
            <p class="muted select-hint">{m['faas.select_hint']()}</p>
          {/if}
        </div>
      </div>
    {/snippet}
  </StateView>
</section>

<style>
  .functions {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  header h1 {
    margin: 0 0 0.25rem;
  }
  .muted {
    color: var(--fg-muted);
    margin: 0;
  }
  .small {
    font-size: 0.8rem;
  }
  .mono {
    font-family: var(--mono, ui-monospace, monospace);
  }
  .grid {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.9fr);
    gap: 1rem;
    align-items: start;
  }
  .list-col,
  .detail-col {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    min-width: 0;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
  }
  h3 {
    margin: 0 0 0.25rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  h4 {
    margin: 0.6rem 0 0.2rem;
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  .fn-list {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: 6px;
    max-height: 20rem;
    overflow-y: auto;
  }
  .fn-opt {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    border-bottom: 1px solid var(--border);
    color: var(--fg);
    padding: 0.4rem 0.6rem;
    cursor: pointer;
    font-size: 0.88rem;
  }
  .fn-opt:hover {
    background: rgba(255, 255, 255, 0.04);
  }
  .fn-opt.selected {
    background: rgba(122, 162, 247, 0.14);
  }
  .dot {
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 999px;
    border: 1px solid var(--fg-muted);
    flex: none;
  }
  .dot.on {
    background: var(--ok, #4caf50);
    border-color: var(--ok, #4caf50);
  }
  .fn-name {
    flex: 1;
    font-weight: 600;
  }
  .fn-trigger,
  .fn-ver {
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .fn-ver {
    font-variant-numeric: tabular-nums;
  }
  .create {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .create label,
  .config label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
  }
  input,
  select,
  textarea {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.35rem 0.5rem;
    font-size: 0.88rem;
  }
  textarea {
    font-family: var(--mono, monospace);
    resize: vertical;
  }
  input:disabled,
  select:disabled,
  textarea:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .inline-err {
    margin: 0;
    color: var(--danger);
    font-size: 0.8rem;
  }
  .actions,
  .save-row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .hint {
    font-size: 0.78rem;
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.3rem 0.9rem;
    cursor: pointer;
    font-size: 0.85rem;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
  }
  button:disabled {
    color: var(--fg-muted);
    cursor: not-allowed;
  }
  button.primary {
    background: var(--accent);
    border-color: var(--accent);
    color: var(--bg, #0b0f17);
    font-weight: 600;
  }
  button.primary:disabled {
    background: transparent;
    color: var(--fg-muted);
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  button.link {
    border: none;
    padding: 0.3rem 0.3rem;
    color: var(--fg-muted);
  }
  .detail {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.75rem;
  }
  .detail-head {
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.4rem;
  }
  .detail-head h2 {
    font-size: 1.15rem;
  }
  .badge {
    font-size: 0.7rem;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--fg-muted);
  }
  .badge.on {
    color: var(--ok, #4caf50);
    border-color: var(--ok, #4caf50);
  }
  .badge.dirty {
    color: var(--warn, #d08770);
    border-color: var(--warn, #d08770);
  }
  .editor {
    min-height: 8rem;
  }
  .config {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }
  .row {
    display: flex;
    gap: 0.6rem;
    align-items: flex-end;
    flex-wrap: wrap;
  }
  .row.wrap {
    align-items: center;
  }
  .row .grow,
  .webhook .grow {
    flex: 1;
  }
  .row input[type='number'] {
    width: 6rem;
  }
  .webhook {
    display: flex;
    gap: 0.6rem;
    align-items: flex-end;
    flex-wrap: wrap;
  }
  .enabled-toggle {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  .enabled-toggle button.on {
    border-color: var(--ok, #4caf50);
    color: var(--ok, #4caf50);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .flabel {
    text-transform: uppercase;
    font-size: 0.72rem;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  .secret-picker {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 1rem;
  }
  .secret-picker label {
    flex-direction: row;
    align-items: center;
    gap: 0.3rem;
    color: var(--fg);
  }
  .chips {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-wrap: wrap;
    gap: 0.3rem;
  }
  .chip {
    font-size: 0.78rem;
    padding: 0.05rem 0.5rem;
    border-radius: 4px;
    border: 1px solid var(--border);
  }
  .save-row .blast {
    margin-left: auto;
  }
  .caps {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
  }
  .caps summary {
    cursor: pointer;
    font-size: 0.85rem;
    font-weight: 600;
  }
  .caplist {
    list-style: none;
    margin: 0.3rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .caplist li {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }
  .caplist code {
    font-family: var(--mono, monospace);
    font-size: 0.8rem;
  }
  .doc {
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .bindings {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .bind-chip {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
  }
  .chip-x {
    border: none;
    background: transparent;
    color: var(--fg-muted);
    padding: 0 0.1rem;
    cursor: pointer;
    font-size: 0.85rem;
  }
  .chip-x:hover:not(:disabled) {
    color: var(--danger);
  }
  .bind-row {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .blast-warn {
    margin: 0;
    color: var(--warn, #d08770);
    font-size: 0.82rem;
  }
  .testrun {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
  }
  .testrun code {
    font-family: var(--mono, monospace);
    font-size: 0.8rem;
  }
  .result {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    border-top: 1px solid var(--border);
    padding-top: 0.5rem;
  }
  .result-head {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .badge.err {
    color: var(--danger);
    border-color: var(--danger);
  }
  .log ul {
    list-style: none;
    margin: 0.2rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    font-family: var(--mono, monospace);
    font-size: 0.8rem;
  }
  .log .lvl {
    color: var(--fg-muted);
    text-transform: uppercase;
    font-size: 0.68rem;
    margin-right: 0.4rem;
  }
  .log li.log-error .lvl,
  .log li.log-error {
    color: var(--danger);
  }
  .log li.log-warn .lvl {
    color: var(--warn, #d08770);
  }
  .detail-error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    align-items: flex-start;
  }
  .detail-error p {
    margin: 0;
    color: var(--danger);
  }
  .select-hint,
  .empty {
    padding: 0.5rem 0;
  }
  .token-banner {
    border: 1px solid var(--warn, #d08770);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .token-banner p {
    margin: 0;
    font-size: 0.85rem;
  }
  .token {
    font-family: var(--mono, monospace);
    font-size: 0.82rem;
    word-break: break-all;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.3rem 0.5rem;
  }
  .dismiss {
    align-self: flex-start;
  }
</style>
