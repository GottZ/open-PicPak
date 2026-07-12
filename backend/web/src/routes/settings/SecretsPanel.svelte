<script lang="ts">
  // Secrets-KV V1 core (design 02-settings-spa §4/§5/§7-W2). Only ever mounted by
  // SettingsHome AFTER the admin-gate confirms is_admin (§8 E4) — so the LIST-load
  // below fires no earlier than that. Meta-table (name/key_version/created_at/
  // rotated_at) + a PUT form (name/value) + a two-step-armed DELETE.
  //
  // WRITE-ONLY SECURITY CORE (§5 B1/B2/B3) — the invariant this file exists to
  // hold:
  //   - B1: `value` is NEVER drafted (no saveDraft/loadDraft import below — pinned
  //     by settings.test.ts N3) and NEVER added to useDirtyGuard. It is reset to
  //     '' the instant a PUT succeeds (below) so it does not linger in this
  //     component's own state either.
  //   - B2: `value` is never read back from a response (the server never returns
  //     it, secrets_api.go:56) and never interpolated into a toast, an inline
  //     error, or a console call. Every notify.*/error string below is built from
  //     {name, action} or a settingsErrorText(err) code mapping — never value.
  //   - B3: the table renders exactly Meta's four fields — no value length, no
  //     fingerprint, nothing derived from the secret's content.
  //   - B4: `name` is operator-free-text (secrets.go:32 permits `._-`); it renders
  //     as a text node ({name}) — {@html} is banned (B6, pinned by ota.test.ts's
  //     sibling gate + scripts/lint-no-html.ts structurally for every .svelte
  //     file); settings.test.ts N4 pins it locally too.
  //   - B7/E3: DELETE is unwiderruflich (no break-glass restore of a gone row) and
  //     degrades bound functions SILENTLY (fail-open `continue` on
  //     ErrSecretNotFound, render.go:66-83) rather than breaking them audibly —
  //     the two-step-arm's confirm label (settings.secrets.delete_confirm) names
  //     the secret AND the silent-undefined degradation, not a "break".
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import {
    isValidSecretName,
    listSecrets,
    putSecret,
    deleteSecret,
    settingsErrorText,
  } from '../../lib/settings/secrets'
  import type { SecretsResponse } from '../../lib/api/types'
  import { m } from '../../paraglide/messages.js'

  const secrets = new Resource<SecretsResponse>(() => listSecrets())

  const affordance = $derived(mutationAffordance(session.is_admin))

  // ---- Put form ----
  let name = $state('')
  let value = $state('')
  let putError = $state<string | null>(null)
  let putting = $state(false)

  const nameLooksValid = $derived(isValidSecretName(name))
  // client-side pre-check only (§4 Put-Flow 1) — the server's 422 invalid_name
  // (secrets_api.go:28) stays authoritative regardless of this gate.
  const canSubmit = $derived(!affordance.disabled && !putting && value !== '' && nameLooksValid)

  async function doPut(e: Event): Promise<void> {
    e.preventDefault()
    if (!session.is_admin || putting || value === '' || !nameLooksValid) return
    putting = true
    putError = null
    try {
      const res = await putSecret(name.trim(), value)
      // B1: leer den value-State SOFORT nach Erfolg — er überlebt den Submit nicht.
      value = ''
      notify.success(
        res.action === 'created'
          ? m['settings.secrets.created']({ name: res.name })
          : m['settings.secrets.rotated']({ name: res.name }),
      )
      await secrets.reload()
    } catch (err) {
      putError = settingsErrorText(err) // Feld-Fehler; value bleibt NIE im Fehlertext (§5 B2)
    } finally {
      putting = false
    }
  }

  // ---- Delete (two-step-arm, Muster FunctionsEditor.svelte:305-332) ----
  let armedName = $state<string | null>(null)
  let deleting = $state(false)

  async function doDelete(secretName: string): Promise<void> {
    if (!session.is_admin || deleting) return
    if (armedName !== secretName) {
      armedName = secretName // erster Klick armt, KEIN Request (E8/B7)
      return
    }
    deleting = true
    try {
      await deleteSecret(secretName)
      notify.success(m['settings.secrets.deleted']({ name: secretName }))
      await secrets.reload()
    } catch (err) {
      notify.error(settingsErrorText(err))
    } finally {
      deleting = false
      armedName = null
    }
  }

  onMount(() => void secrets.load())
</script>

<section class="secrets" aria-label={m['settings.secrets.heading']()}>
  <h2>{m['settings.secrets.heading']()}</h2>

  <form class="put" onsubmit={doPut}>
    <label class="field">
      <span class="field-label">{m['settings.secrets.name_label']()}</span>
      <input
        type="text"
        bind:value={name}
        maxlength="128"
        spellcheck="false"
        autocomplete="off"
        disabled={affordance.disabled || putting}
        aria-label={m['settings.secrets.name_label']()}
      />
    </label>
    <p class="muted small">{m['settings.secrets.name_hint']()}</p>
    <label class="field">
      <span class="field-label">{m['settings.secrets.value_label']()}</span>
      <textarea
        bind:value
        rows="3"
        spellcheck="false"
        disabled={affordance.disabled || putting}
        aria-label={m['settings.secrets.value_label']()}
      ></textarea>
    </label>
    {#if name !== '' && !nameLooksValid}
      <p class="inline-err">{m['settings.secrets.error.invalid_name']()}</p>
    {/if}
    {#if putError}
      <p class="inline-err" role="alert">{putError}</p>
    {/if}
    <button
      type="submit"
      class="primary"
      disabled={!canSubmit}
      title={affordance.disabled ? affordance.title : ''}
      aria-disabled={affordance['aria-disabled'] || !canSubmit}
    >
      {m['settings.secrets.put']()}
    </button>
  </form>

  <StateView resource={secrets} emptyText={m['settings.secrets.empty']()} isEmpty={(data) => data.secrets.length === 0}>
    {#snippet ready(data)}
      <table>
        <thead>
          <tr>
            <th>{m['settings.secrets.col.name']()}</th>
            <th>{m['settings.secrets.col.key_version']()}</th>
            <th>{m['settings.secrets.col.created']()}</th>
            <th>{m['settings.secrets.col.rotated']()}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {#each data.secrets as s (s.name)}
            <tr>
              <td>{s.name}</td>
              <td>{s.key_version}</td>
              <td>{s.created_at}</td>
              <td>{s.rotated_at ?? '—'}</td>
              <td class="actions">
                <button
                  type="button"
                  class="danger"
                  disabled={affordance.disabled || deleting}
                  title={affordance.disabled ? affordance.title : ''}
                  onclick={() => doDelete(s.name)}
                >
                  {armedName === s.name ? m['settings.secrets.delete_confirm']({ name: s.name }) : m['settings.secrets.delete']()}
                </button>
                {#if armedName === s.name}
                  <button type="button" class="link" onclick={() => (armedName = null)}>
                    {m['settings.secrets.delete_cancel']()}
                  </button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/snippet}
  </StateView>
</section>

<style>
  .secrets {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .put {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem;
    max-width: 32rem;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
  }
  input,
  textarea {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.35rem 0.5rem;
    font-size: 0.88rem;
    font-family: inherit;
  }
  textarea {
    font-family: var(--mono, monospace);
    resize: vertical;
  }
  input:disabled,
  textarea:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .muted {
    color: var(--fg-muted);
    margin: 0;
  }
  .small {
    font-size: 0.78rem;
  }
  .inline-err {
    margin: 0;
    color: var(--danger);
    font-size: 0.8rem;
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.3rem 0.9rem;
    cursor: pointer;
    font-size: 0.85rem;
    align-self: flex-start;
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
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  button.link {
    border: none;
    padding: 0.3rem 0.3rem;
    color: var(--fg-muted);
    align-self: auto;
  }
</style>
