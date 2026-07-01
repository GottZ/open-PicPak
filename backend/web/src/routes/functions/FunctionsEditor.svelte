<script lang="ts">
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import StateView from '../../lib/StateView.svelte'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import {
    TRIGGER_TYPES,
    newFunctionError,
    blastRadiusLabel,
    type NewFunctionDraft,
  } from '../../lib/faas/functions'
  import type {
    TriggerType,
    FunctionsResponse,
    FunctionResponse,
    FunctionDetail,
    CreateFunctionResponse,
  } from '../../lib/faas/types'

  // FaaS function editor (Design 25, W1) — the function list + CRUD (create / enable-toggle / delete) +
  // a read-only view of a function's source, config and bindings. NO execution ships here: the code
  // editor (CM6) lands in W2, test-run in W3, the bind picker in W4. Every function source, config value,
  // secret NAME, egress host and serial renders as a TEXT NODE (Svelte auto-escapes {…}); {@html} is
  // banned (D19.10 — function source + operator input are foreign/attacker-influenced). The server stays
  // authoritative on every mutation via requireAdmin (D25.11); the read-only affordance is cosmetic.

  const SOURCE_PLACEHOLDER = 'export default async (ctx, cap) => ({ image })'

  const functions = new Resource<FunctionsResponse>(() => apiFetch<FunctionsResponse>('/api/functions'))
  void functions.load()

  // selected function detail (loaded on demand — keyed by selection, so managed by hand rather than a
  // Resource). detail carries the source + config + the serials bound to it (its blast radius).
  let selectedId = $state<number | null>(null)
  let detail = $state<FunctionResponse | null>(null)
  let detailStatus = $state<'idle' | 'loading' | 'error'>('idle')
  let detailError = $state<string | null>(null)

  // create form
  let newName = $state('')
  let newTrigger = $state<TriggerType>('render')
  let newSource = $state('')
  let creating = $state(false)

  // a webhook function's token is shown EXACTLY ONCE on create (D24.13); hold it in a dismissible banner
  // the operator copies — it is never recoverable (rotate to replace).
  let mintedToken = $state<{ name: string; token: string } | null>(null)

  // busy guards so a double-click can't fire two mutations.
  let mutating = $state(false)

  // server stays authoritative via requireAdmin; this is the cosmetic gate (D25.11 / D19.6).
  const affordance = $derived(mutationAffordance(session.is_admin))
  const draft = $derived<NewFunctionDraft>({ name: newName, source: newSource, triggerType: newTrigger })
  const createError = $derived(newFunctionError(draft))

  async function loadDetail(id: number): Promise<void> {
    selectedId = id
    detailStatus = 'loading'
    detailError = null
    try {
      detail = await apiFetch<FunctionResponse>(`/api/functions/${id}`)
      detailStatus = 'idle'
    } catch (e) {
      detail = null
      detailError = toApiError(e).message
      detailStatus = 'error'
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
        body: JSON.stringify({ name: newName.trim(), source: newSource, trigger_type: newTrigger }),
      })
      notify.success(`created function “${res.name}” (disabled — enable it when ready)`)
      if (res.webhook_token) mintedToken = { name: res.name, token: res.webhook_token }
      newName = ''
      newSource = ''
      newTrigger = 'render'
      await functions.load()
      await loadDetail(res.id)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      creating = false
    }
  }

  async function toggleEnabled(fn: FunctionDetail): Promise<void> {
    if (mutating || !session.is_admin) return
    mutating = true
    try {
      await apiFetch(`/api/functions/${fn.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ enabled: !fn.enabled }),
      })
      notify.success(`${fn.name} ${fn.enabled ? 'disabled' : 'enabled'}`)
      await reloadAll(true)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
    }
  }

  // delete is armed by a first click (the confirm shows the blast radius), committed by the second — no
  // browser confirm() dialog; the cascade drops the function's device bindings + last-good rows (0009).
  let deleteArmed = $state(false)
  $effect(() => {
    // disarm whenever the selection changes.
    void selectedId
    deleteArmed = false
  })

  async function doDelete(fn: FunctionDetail): Promise<void> {
    if (mutating || !session.is_admin) return
    if (!deleteArmed) {
      deleteArmed = true
      return
    }
    mutating = true
    try {
      await apiFetch(`/api/functions/${fn.id}`, { method: 'DELETE' })
      notify.success(`deleted function “${fn.name}”`)
      selectedId = null
      detail = null
      await functions.load()
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      mutating = false
      deleteArmed = false
    }
  }

  function configText(cfg: Record<string, unknown>): string {
    return Object.keys(cfg).length === 0 ? '' : JSON.stringify(cfg, null, 2)
  }
</script>

<section class="functions">
  <header>
    <h1>Functions</h1>
    <p class="muted">
      Author render/schedule/webhook functions for the FaaS engine. This wave ships the function list,
      create/enable/delete, and a read-only view — the code editor, test-run and preview arrive in later
      waves. A new function is created <strong>disabled</strong>; enable it when it is ready to serve.
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
              <input
                type="text"
                bind:value={newName}
                placeholder="lowercase-slug"
                autocomplete="off"
                spellcheck="false"
              />
            </label>
            <label>
              trigger
              <select bind:value={newTrigger}>
                {#each TRIGGER_TYPES as t (t)}
                  <option value={t}>{t}</option>
                {/each}
              </select>
            </label>
            <label class="src-label">
              source
              <textarea
                bind:value={newSource}
                rows="4"
                placeholder={SOURCE_PLACEHOLDER}
                spellcheck="false"
              ></textarea>
            </label>
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
                <span class="muted hint">a token is minted + shown once on create</span>
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
            {@const cfg = configText(fn.trigger_config)}
            <div class="detail">
              <div class="detail-head">
                <h2>{fn.name}</h2>
                <span class="badge">{fn.trigger_type}</span>
                <span class="badge" class:on={fn.enabled}>{fn.enabled ? 'enabled' : 'disabled'}</span>
                <span class="muted">v{fn.version}</span>
              </div>

              <div class="detail-actions">
                <button
                  type="button"
                  disabled={affordance.disabled || mutating}
                  title={affordance.disabled ? affordance.title : ''}
                  aria-disabled={affordance['aria-disabled']}
                  onclick={() => toggleEnabled(fn)}
                >
                  {fn.enabled ? 'Disable' : 'Enable'}
                </button>
                <button
                  type="button"
                  class="danger"
                  disabled={affordance.disabled || mutating}
                  title={affordance.disabled ? affordance.title : ''}
                  aria-disabled={affordance['aria-disabled']}
                  onclick={() => doDelete(fn)}
                >
                  {deleteArmed
                    ? `confirm delete (${blastRadiusLabel(detail.bound_serials.length)})`
                    : 'Delete'}
                </button>
                {#if deleteArmed}
                  <button type="button" class="link" onclick={() => (deleteArmed = false)}>cancel</button>
                {/if}
              </div>

              <div class="field">
                <span class="flabel">bound devices ({blastRadiusLabel(detail.bound_serials.length)})</span>
                {#if detail.bound_serials.length > 0}
                  <ul class="chips">
                    {#each detail.bound_serials as s (s)}
                      <li class="chip mono">{s}</li>
                    {/each}
                  </ul>
                  <p class="muted small">Editing this function's source will change the next frame on every bound device.</p>
                {:else}
                  <p class="muted small">Not bound to any device — nothing renders it yet.</p>
                {/if}
              </div>

              <div class="field">
                <span class="flabel">source</span>
                <pre class="code">{fn.source}</pre>
              </div>

              <div class="field">
                <span class="flabel">trigger config</span>
                {#if cfg === ''}
                  <p class="muted small">(defaults)</p>
                {:else}
                  <pre class="code">{cfg}</pre>
                {/if}
              </div>

              <div class="field two">
                <div>
                  <span class="flabel">secret bindings</span>
                  {#if fn.secret_bindings.length > 0}
                    <ul class="chips">
                      {#each fn.secret_bindings as name (name)}
                        <li class="chip mono">{name}</li>
                      {/each}
                    </ul>
                  {:else}
                    <p class="muted small">none</p>
                  {/if}
                </div>
                <div>
                  <span class="flabel">egress allow</span>
                  {#if fn.egress_allow.length > 0}
                    <ul class="chips">
                      {#each fn.egress_allow as host (host)}
                        <li class="chip mono">{host}</li>
                      {/each}
                    </ul>
                  {:else}
                    <p class="muted small">none (no outbound network)</p>
                  {/if}
                </div>
              </div>
            </div>
          {:else}
            <p class="muted select-hint">Select a function to view its source, config and bindings.</p>
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
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.6fr);
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
  .fn-list {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: 6px;
    max-height: 22rem;
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
  .fn-trigger {
    color: var(--fg-muted);
    font-size: 0.78rem;
  }
  .fn-ver {
    color: var(--fg-muted);
    font-variant-numeric: tabular-nums;
    font-size: 0.78rem;
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
  .src-label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
  }
  .create input,
  .create select,
  .create textarea {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.35rem 0.5rem;
    font-size: 0.88rem;
  }
  .create textarea {
    font-family: var(--mono, monospace);
    resize: vertical;
  }
  .inline-err {
    margin: 0;
    color: var(--danger);
    font-size: 0.8rem;
  }
  .actions,
  .detail-actions {
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
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .field.two {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
  }
  .flabel {
    text-transform: uppercase;
    font-size: 0.72rem;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  .code {
    margin: 0;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.6rem;
    font-family: var(--mono, monospace);
    font-size: 0.82rem;
    white-space: pre-wrap;
    word-break: break-word;
    max-height: 22rem;
    overflow: auto;
  }
  .chips {
    list-style: none;
    margin: 0.2rem 0 0;
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
