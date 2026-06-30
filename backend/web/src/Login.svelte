<script lang="ts">
  import { session } from './lib/auth.svelte'
  import { toApiError, type ApiError } from './lib/api'

  let key = $state('')
  let busy = $state(false)
  let error = $state<ApiError | null>(null)

  const submittable = $derived(!busy && key.trim() !== '')

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    if (!submittable) return
    busy = true
    error = null
    try {
      await session.login(key)
      key = ''
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }
</script>

<div class="screen">
  <form class="card" onsubmit={submit}>
    <header>
      <h1>open-picpak</h1>
      <p class="tagline">operator control plane</p>
    </header>

    {#if session.notice}
      <p class="notice" role="status">{session.notice}</p>
    {/if}

    <label class="field-label" for="api-key">Operator key</label>
    <!-- eslint-disable-next-line -->
    <input
      id="api-key"
      type="password"
      autocomplete="off"
      spellcheck="false"
      placeholder="paste an operator key"
      bind:value={key}
      {@attach (node) => node.focus()}
    />

    <button type="submit" disabled={!submittable}>
      {busy ? 'Checking key…' : 'Sign in'}
    </button>

    {#if error}
      <div class="error" role="alert">
        <p>{error.message}</p>
        {#if error.requestId}
          <p class="request-id">request {error.requestId}</p>
        {/if}
      </div>
    {/if}

    <p class="hint">
      The key is held for this tab only (sessionStorage) and sent as a
      <code>Bearer</code> token to <code>/api/*</code>.
    </p>
  </form>
</div>

<style>
  .screen {
    min-height: 100dvh;
    display: grid;
    place-items: center;
    padding: 1.5rem;
  }
  .card {
    width: min(24rem, 100%);
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 2rem;
  }
  header {
    margin-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.5rem;
    font-weight: 600;
  }
  .tagline {
    margin: 0.25rem 0 0;
    color: var(--fg-muted);
    font-size: 0.9rem;
  }
  .notice {
    margin: 0;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--warn);
    border-radius: 6px;
    color: var(--warn);
    font-size: 0.85rem;
  }
  .field-label {
    font-family: monospace;
    font-size: 0.72rem;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    color: var(--fg-muted);
  }
  input {
    font-family: monospace;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.5rem 0.75rem;
  }
  input:focus {
    outline: none;
    border-color: var(--accent);
  }
  button {
    margin-top: 0.25rem;
    border: 1px solid var(--accent);
    color: var(--accent);
    background: transparent;
    border-radius: 6px;
    padding: 0.5rem;
    cursor: pointer;
  }
  button:hover:not(:disabled) {
    background: rgba(122, 162, 247, 0.12);
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    margin-top: 0.25rem !important;
    font-family: monospace;
    font-size: 0.75rem;
    color: var(--fg-muted) !important;
  }
  .hint {
    margin: 0.5rem 0 0;
    color: var(--fg-muted);
    font-size: 0.78rem;
    line-height: 1.45;
  }
  code {
    font-family: monospace;
  }
</style>
