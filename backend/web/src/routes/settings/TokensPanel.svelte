<script lang="ts">
  // API-Token-Panel (design 02-settings-spa §7-W4). Wie SecretsPanel: nur ueber SettingsHome AFTER dem
  // admin-Gate gemountet (§8 E4) — alle drei Routen POST/GET/DELETE /api/tokens sind RequireAdmin
  // (token_http.go:26-28), also gilt dieselbe B5-Faelle wie bei Secrets: kein separater "needs admin"-
  // Zustand hier noetig, SettingsHome traegt ihn bereits fuer den gesamten admin-Zweig.
  //
  // ONCE-SHOWN-SECURITY-KERN (§7-W4, Muster FunctionsEditor.svelte:292-296 mintedToken):
  //   - Der geminte Klartext-Token lebt AUSSCHLIESSLICH im lokalen `mintedToken`-$state unten. Er wird
  //     NIE gedraftet (kein saveDraft/loadDraft-Import — pinned settings.test.ts), NIE geloggt und NIE
  //     in einen notify.*-Aufruf interpoliert (der Erfolgs-Toast traegt nur {label}, pinned N2).
  //   - Er verschwindet beim naechsten Mint (mintedToken wird vor dem Request auf null gesetzt) und beim
  //     Unmount des Panels (kein Modul-/Store-State dahinter, nur Komponenten-lokal).
  //   - Die Meta-Liste (list()) traegt strukturell nie ein Token-/Secret-Feld — apitoken.List
  //     (store.go:172-187) projiziert nie secret_hash; TokenMeta hat deshalb kein value/token-Feld.
  //
  // SCOPE-PFLICHT (Gate a-c, §7-W4): mint() akzeptiert nur apitoken.KnownScopes (store.go:51-52) — ein
  // unbekannter Scope ist 422 unknown_scope; das Panel bildet diesen Code als Feld-Fehler UNTER dem
  // Scope-Multiselect ab (tokenErrorField), nicht als generischen Toast.
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { session } from '../../lib/auth.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { notify } from '../../lib/toasts.svelte'
  import {
    KNOWN_SCOPES,
    listTokens,
    mintToken,
    revokeToken,
    mintFormValid,
    datetimeLocalToRFC3339,
    tokenErrorField,
    tokensErrorText,
    tokenStatusText,
    type TokensResponse,
    type TokenScope,
  } from '../../lib/settings/tokens'
  import { m } from '../../paraglide/messages.js'

  const tokens = new Resource<TokensResponse>(() => listTokens())

  const affordance = $derived(mutationAffordance(session.is_admin))

  // ---- Mint form ----
  let label = $state('')
  let scopes = $state<TokenScope[]>([])
  let expiresLocal = $state('') // <input type="datetime-local"> Rohwert, lokale Zeit
  let mintError = $state<string | null>(null)
  let mintErrorField = $state<'label' | 'scopes' | 'expires_at' | null>(null)
  let minting = $state(false)

  const canSubmit = $derived(!affordance.disabled && !minting && mintFormValid({ label, scopes }))

  function toggleScope(s: TokenScope): void {
    scopes = scopes.includes(s) ? scopes.filter((x) => x !== s) : [...scopes, s]
  }

  // Once-shown: der geminte Klartext lebt NUR hier — nie Draft/Log/localStorage (siehe Kopf-Kommentar).
  let mintedToken = $state<{ label: string; token: string } | null>(null)
  let copied = $state(false)

  async function doMint(e: Event): Promise<void> {
    e.preventDefault()
    if (!session.is_admin || minting || !mintFormValid({ label, scopes })) return
    minting = true
    mintError = null
    mintErrorField = null
    mintedToken = null
    try {
      const res = await mintToken({
        label: label.trim(),
        scopes,
        expiresAt: datetimeLocalToRFC3339(expiresLocal),
      })
      // Once-shown: res.token fliesst NUR in den lokalen State — nie in notify/log (§7-W4).
      mintedToken = { label: res.label, token: res.token }
      copied = false
      notify.success(m['settings.tokens.minted']({ label: res.label }))
      label = ''
      scopes = []
      expiresLocal = ''
      await tokens.reload()
    } catch (err) {
      mintError = tokensErrorText(err) // Feld-Fehler; der Token selbst bleibt NIE im Fehlertext
      mintErrorField = tokenErrorField(err)
    } finally {
      minting = false
    }
  }

  async function copyToken(): Promise<void> {
    if (!mintedToken) return
    try {
      await navigator.clipboard.writeText(mintedToken.token)
      copied = true
    } catch {
      // Clipboard-API kann in einem unsicheren Kontext fehlen — der Token bleibt sichtbar zum manuellen
      // Kopieren; kein Fehler-Toast fuer einen rein kosmetischen Fallback.
    }
  }

  // ---- Revoke (two-step-arm, Muster SecretsPanel.svelte §5 B7/E3) ----
  let armedId = $state<number | null>(null)
  let revoking = $state(false)

  async function doRevoke(id: number): Promise<void> {
    if (!session.is_admin || revoking) return
    if (armedId !== id) {
      armedId = id // erster Klick armt, KEIN Request
      return
    }
    revoking = true
    try {
      await revokeToken(id)
      notify.success(m['settings.tokens.revoked']())
      await tokens.reload()
    } catch (err) {
      notify.error(tokensErrorText(err))
    } finally {
      revoking = false
      armedId = null
    }
  }

  onMount(() => void tokens.load())
</script>

<section class="tokens" aria-label={m['settings.tokens.heading']()}>
  <h2>{m['settings.tokens.heading']()}</h2>

  {#if mintedToken}
    <div class="token-banner" role="alert">
      <p>{m['settings.tokens.once_shown']({ label: mintedToken.label })}</p>
      <code class="token">{mintedToken.token}</code>
      <div class="banner-actions">
        <button type="button" onclick={copyToken}>
          {copied ? m['settings.tokens.copied']() : m['settings.tokens.copy']()}
        </button>
        <button type="button" class="link" onclick={() => (mintedToken = null)}>
          {m['settings.tokens.dismiss']()}
        </button>
      </div>
    </div>
  {/if}

  <form class="mint" onsubmit={doMint}>
    <label class="field">
      <span class="field-label">{m['settings.tokens.label_label']()}</span>
      <input
        type="text"
        bind:value={label}
        maxlength="128"
        spellcheck="false"
        autocomplete="off"
        disabled={affordance.disabled || minting}
        aria-label={m['settings.tokens.label_label']()}
      />
    </label>
    {#if mintErrorField === 'label'}
      <p class="inline-err" role="alert">{mintError}</p>
    {/if}

    <fieldset class="scopes">
      <legend>{m['settings.tokens.scopes_label']()}</legend>
      {#each KNOWN_SCOPES as s (s)}
        <label class="scope-opt">
          <input
            type="checkbox"
            checked={scopes.includes(s)}
            disabled={affordance.disabled || minting}
            onchange={() => toggleScope(s)}
          />
          <span class="mono">{s}</span>
        </label>
      {/each}
    </fieldset>
    {#if mintErrorField === 'scopes'}
      <p class="inline-err" role="alert">{mintError}</p>
    {/if}

    <label class="field">
      <span class="field-label">{m['settings.tokens.expires_label']()}</span>
      <input
        type="datetime-local"
        bind:value={expiresLocal}
        disabled={affordance.disabled || minting}
        aria-label={m['settings.tokens.expires_label']()}
      />
    </label>
    <p class="muted small">{m['settings.tokens.expires_hint']()}</p>
    {#if mintErrorField === 'expires_at'}
      <p class="inline-err" role="alert">{mintError}</p>
    {/if}

    {#if mintError && mintErrorField === null}
      <p class="inline-err" role="alert">{mintError}</p>
    {/if}

    <button
      type="submit"
      class="primary"
      disabled={!canSubmit}
      title={affordance.disabled ? affordance.title : ''}
      aria-disabled={affordance['aria-disabled'] || !canSubmit}
    >
      {minting ? m['settings.tokens.minting']() : m['settings.tokens.mint']()}
    </button>
  </form>

  <StateView resource={tokens} emptyText={m['settings.tokens.empty']()} isEmpty={(data) => data.tokens.length === 0}>
    {#snippet ready(data)}
      <table>
        <thead>
          <tr>
            <th>{m['settings.tokens.col.label']()}</th>
            <th>{m['settings.tokens.col.scopes']()}</th>
            <th>{m['settings.tokens.col.status']()}</th>
            <th>{m['settings.tokens.col.created']()}</th>
            <th>{m['settings.tokens.col.expires']()}</th>
            <th>{m['settings.tokens.col.last_used']()}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {#each data.tokens as t (t.id)}
            <tr>
              <td>{t.label}</td>
              <td class="mono">{t.scopes.join(', ')}</td>
              <td>{tokenStatusText(t.status)}</td>
              <td>{t.created_at}</td>
              <td>{t.expires_at ?? '—'}</td>
              <td>{t.last_used ?? '—'}</td>
              <td class="actions">
                <button
                  type="button"
                  class="danger"
                  disabled={affordance.disabled || revoking}
                  title={affordance.disabled ? affordance.title : ''}
                  onclick={() => doRevoke(t.id)}
                >
                  {armedId === t.id ? m['settings.tokens.revoke_confirm']({ label: t.label }) : m['settings.tokens.revoke']()}
                </button>
                {#if armedId === t.id}
                  <button type="button" class="link" onclick={() => (armedId = null)}>
                    {m['settings.tokens.revoke_cancel']()}
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
  .tokens {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  .mint {
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
  fieldset.scopes {
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.6rem;
    margin: 0;
  }
  fieldset.scopes legend {
    font-size: 0.8rem;
    color: var(--fg-muted);
    padding: 0 0.2rem;
  }
  .scope-opt {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.85rem;
    color: var(--fg);
  }
  input {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.35rem 0.5rem;
    font-size: 0.88rem;
    font-family: inherit;
  }
  input:disabled {
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
  .mono {
    font-family: var(--mono, ui-monospace, monospace);
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
  button.link {
    border: none;
    padding: 0.3rem 0.3rem;
    color: var(--fg-muted);
    align-self: auto;
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
  .token-banner {
    border: 1px solid var(--warn, #d08770);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    max-width: 32rem;
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
  .banner-actions {
    display: flex;
    gap: 0.5rem;
  }
</style>
