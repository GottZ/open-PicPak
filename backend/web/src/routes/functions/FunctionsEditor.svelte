<script lang="ts">
  import { onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import StateView from '../../lib/StateView.svelte'
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
  import type {
    TriggerType,
    FunctionsResponse,
    FunctionResponse,
    CreateFunctionResponse,
    SecretsResponse,
    ConfigResponse,
  } from '../../lib/faas/types'

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

  $effect(() => {
    const el = editorEl
    if (el === mountedNode) return
    handle?.destroy()
    handle = null
    mountedNode = el
    if (el) {
      handle = createFaasEditor({
        parent: el,
        doc: source,
        getBound: () => boundSecrets,
        readOnly: !session.is_admin,
        onChange: (d) => {
          source = d
          if (selectedId !== null) saveDraft(`faas:${selectedId}`, d)
        },
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
      notify.success(`created “${res.name}” (disabled — author it, then enable)`)
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
          ? `saved “${fn.name}” (version bumped) — next frame changes on ${blastRadiusLabel(bound)}`
          : `saved “${fn.name}” (version bumped)`,
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
      notify.success(`${fn.name} ${fn.enabled ? 'disabled' : 'enabled'}`)
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
      notify.success(`rotated webhook token for ${fn.name}`)
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
      notify.success(`deleted “${fn.name}”`)
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

  function toggleSecret(name: string): void {
    boundSecrets = boundSecrets.includes(name)
      ? boundSecrets.filter((n) => n !== name)
      : [...boundSecrets, name]
  }

  const hookURL = $derived(detail ? webhookURL(webhookBase, detail.function.name) : null)
</script>

<section class="functions">
  <header>
    <h1>Functions</h1>
    <p class="muted">
      Author render / schedule / webhook functions for the FaaS engine. Edit the source, configure the
      trigger / egress / secret bindings / dither, and enable when ready. Test-run + frame preview arrive
      in the next wave — this surface authors, it does not execute.
    </p>
  </header>

  {#if mintedToken}
    <div class="token-banner" role="alert">
      <p>
        Webhook token for <strong>{mintedToken.name}</strong> — shown once, not recoverable. Hand it to the
        caller; rotate to replace.
      </p>
      <code class="token">{mintedToken.token}</code>
      <button type="button" class="dismiss" onclick={() => (mintedToken = null)}>dismiss</button>
    </div>
  {/if}

  <StateView resource={functions} isEmpty={() => false} loadingText="loading functions…">
    {#snippet ready(data: FunctionsResponse)}
      <div class="grid">
        <div class="list-col">
          <h2>functions</h2>
          {#if data.functions.length === 0}
            <p class="muted empty">No functions yet — create one below.</p>
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
                    <span class="dot" class:on={fn.enabled} title={fn.enabled ? 'enabled' : 'disabled'}></span>
                    <span class="fn-name">{fn.name}</span>
                    <span class="fn-trigger">{fn.trigger_type}</span>
                    <span class="fn-ver">v{fn.version}</span>
                  </button>
                </li>
              {/each}
            </ul>
          {/if}

          <div class="create">
            <h3>create function</h3>
            <label>
              name
              <input type="text" bind:value={newName} placeholder="lowercase-slug" autocomplete="off" spellcheck="false" />
            </label>
            <label>
              trigger
              <select bind:value={newTrigger}>
                {#each TRIGGER_TYPES as tt (tt)}
                  <option value={tt}>{tt}</option>
                {/each}
              </select>
            </label>
            <p class="muted small">Created with a starter template — author the source in the editor.</p>
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
                {creating ? 'creating…' : 'Create'}
              </button>
              {#if newTrigger === 'webhook'}
                <span class="muted hint">a token is minted + shown once</span>
              {/if}
            </div>
          </div>
        </div>

        <div class="detail-col">
          {#if detailStatus === 'loading'}
            <p class="muted" aria-busy="true">loading function…</p>
          {:else if detailStatus === 'error'}
            <div class="detail-error" role="alert">
              <p>{detailError}</p>
              {#if selectedId !== null}
                <button type="button" onclick={() => selectedId !== null && loadDetail(selectedId)}>retry</button>
              {/if}
            </div>
          {:else if detail}
            {@const fn = detail.function}
            <div class="detail">
              <div class="detail-head">
                <h2>{fn.name}</h2>
                <span class="badge">{fn.trigger_type}</span>
                <span class="badge" class:on={fn.enabled}>{fn.enabled ? 'enabled' : 'disabled'}</span>
                <span class="muted">v{fn.version}</span>
                {#if dirty}<span class="badge dirty">unsaved</span>{/if}
              </div>

              {#if !session.is_admin}
                <p class="muted small">Read-only — sign in with an admin key to edit, enable or delete.</p>
              {/if}

              <div class="editor-wrap">
                <div class="editor" bind:this={editorEl}></div>
              </div>

              <div class="config">
                <h3>trigger · {fn.trigger_type}</h3>
                {#if fn.trigger_type === 'render'}
                  <div class="row">
                    <label>
                      mode
                      <select bind:value={trigFields.mode} disabled={affordance.disabled}>
                        <option value="">sync (default)</option>
                        {#each RENDER_MODES as m (m)}<option value={m}>{m}</option>{/each}
                      </select>
                    </label>
                    <label>
                      ttl_s
                      <input type="number" min="0" bind:value={trigFields.ttlS} disabled={affordance.disabled} />
                    </label>
                    <label>
                      interval_s
                      <input type="number" min="0" bind:value={trigFields.intervalS} disabled={affordance.disabled} />
                    </label>
                  </div>
                {:else if fn.trigger_type === 'schedule'}
                  <div class="row">
                    <label class="grow">
                      cron
                      <input
                        type="text"
                        bind:value={trigFields.cron}
                        placeholder="m h dom mon dow"
                        disabled={affordance.disabled}
                        spellcheck="false"
                      />
                    </label>
                    <label>
                      interval_s
                      <input type="number" min="0" bind:value={trigFields.intervalS} disabled={affordance.disabled} />
                    </label>
                  </div>
                  {#if !cronOk}<p class="inline-err">cron must be 5 whitespace-separated fields</p>{/if}
                  <p class="muted small">Cron drives the schedule; interval_s is the M1 fallback.</p>
                {:else}
                  <div class="webhook">
                    {#if hookURL}
                      <label class="grow">
                        inbound URL
                        <input type="text" class="mono" readonly value={hookURL} />
                      </label>
                    {:else}
                      <p class="muted small">Webhook base URL not configured (ADMIN_WEBHOOK_BASE_URL) — the URL is hidden.</p>
                    {/if}
                    <button
                      type="button"
                      disabled={affordance.disabled || mutating}
                      title={affordance.disabled ? affordance.title : ''}
                      onclick={rotateToken}
                    >
                      rotate token
                    </button>
                  </div>
                {/if}

                <div class="row wrap">
                  <label>
                    dither
                    <select bind:value={trigFields.dither} disabled={affordance.disabled}>
                      {#each DITHER_OPTIONS as d (d.value)}<option value={d.value}>{d.label}</option>{/each}
                    </select>
                  </label>
                  <div class="enabled-toggle">
                    <span class="flabel">enabled</span>
                    <button
                      type="button"
                      class:on={fn.enabled}
                      disabled={affordance.disabled || mutating}
                      title={affordance.disabled ? affordance.title : ''}
                      onclick={toggleEnabled}
                    >
                      {fn.enabled ? 'on' : 'off'}
                    </button>
                  </div>
                </div>

                <div class="field">
                  <span class="flabel">egress allow (one host[:port] per line — empty = no outbound network)</span>
                  <textarea
                    bind:value={egressText}
                    rows="2"
                    placeholder="host.example:8123"
                    disabled={affordance.disabled}
                    spellcheck="false"
                  ></textarea>
                </div>

                <div class="field">
                  <span class="flabel">secret bindings (names only — least privilege; a value is never shown)</span>
                  {#if session.is_admin}
                    {#if secretNames.length === 0}
                      <p class="muted small">No secrets defined — add them in Settings (Doc 18).</p>
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
                    <p class="muted small">none</p>
                  {/if}
                </div>

                <div class="save-row">
                  <button
                    type="button"
                    class="primary"
                    disabled={affordance.disabled || saving || !dirty || !cronOk}
                    title={affordance.disabled ? affordance.title : ''}
                    aria-disabled={affordance['aria-disabled'] || !dirty}
                    onclick={doSave}
                  >
                    {saving ? 'saving…' : 'Save'}
                  </button>
                  <button
                    type="button"
                    class="danger"
                    disabled={affordance.disabled || mutating}
                    title={affordance.disabled ? affordance.title : ''}
                    onclick={doDelete}
                  >
                    {deleteArmed ? `confirm delete (${blastRadiusLabel(detail.bound_serials.length)})` : 'Delete'}
                  </button>
                  {#if deleteArmed}<button type="button" class="link" onclick={() => (deleteArmed = false)}>cancel</button>{/if}
                  <span class="muted small blast">bound: {blastRadiusLabel(detail.bound_serials.length)}</span>
                </div>
              </div>

              <details class="caps">
                <summary>capability reference</summary>
                <p class="muted small">
                  The curated worker scope — autocomplete offers exactly these (and this function's bound
                  secrets). Documentation, not a firmware-parity manifest.
                </p>
                <h4>cap.* (capabilities)</h4>
                <ul class="caplist">
                  {#each CAP_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
                <h4>ctx.* (context)</h4>
                <ul class="caplist">
                  {#each CTX_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
                <h4>return</h4>
                <ul class="caplist">
                  {#each RETURN_ENTRIES as c (c.label)}
                    <li><code>{c.detail}</code><span class="doc">{c.info}</span></li>
                  {/each}
                </ul>
              </details>
            </div>
          {:else}
            <p class="muted select-hint">Select a function to author it, or create one.</p>
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
