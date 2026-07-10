<script lang="ts">
  // The one toast region (D19.11), owned by App.svelte. Renders notify.* toasts
  // with auto-dismiss (store-driven), manual close, and a copy-request-id action
  // on errors (greppable in the cmd/admin logs).
  import { notify } from './toasts.svelte'
  import { m } from '../paraglide/messages.js'

  function copy(text: string): void {
    void navigator.clipboard?.writeText(text)
  }
</script>

<div class="toaster" aria-live="polite">
  {#each notify.toasts as t (t.id)}
    <div class="toast toast-{t.kind}" role={t.kind === 'error' ? 'alert' : 'status'}>
      <div class="body">
        <p class="msg">{t.message}</p>
        {#if t.code || t.requestId}
          <p class="meta">
            {#if t.code}<span class="code">{t.code}</span>{/if}
            {#if t.requestId}
              <button class="copy" onclick={() => copy(t.requestId ?? '')}>{m['toast.copy_id']()}</button>
            {/if}
          </p>
        {/if}
      </div>
      <button class="close" aria-label={m['toast.dismiss']()} onclick={() => notify.dismiss(t.id)}>×</button>
    </div>
  {/each}
</div>

<style>
  .toaster {
    position: fixed;
    bottom: 1rem;
    right: 1rem;
    z-index: 50;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    max-width: min(24rem, calc(100vw - 2rem));
  }
  .toast {
    display: flex;
    gap: 0.5rem;
    align-items: flex-start;
    background: var(--surface);
    border: 1px solid var(--border);
    border-left-width: 3px;
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.4);
    font-size: 0.85rem;
  }
  .toast-success {
    border-left-color: var(--ok);
  }
  .toast-info {
    border-left-color: var(--accent);
  }
  .toast-warn {
    border-left-color: var(--warn);
  }
  .toast-error {
    border-left-color: var(--danger);
  }
  .body {
    flex: 1;
  }
  .msg {
    margin: 0;
  }
  .meta {
    margin: 0.3rem 0 0;
    display: flex;
    gap: 0.5rem;
    align-items: center;
    font-size: 0.75rem;
    color: var(--fg-muted);
  }
  .code {
    font-family: monospace;
  }
  .copy {
    background: transparent;
    border: 1px solid var(--border);
    border-radius: 4px;
    color: var(--fg-muted);
    padding: 0 0.4rem;
    cursor: pointer;
    font-size: 0.72rem;
  }
  .copy:hover {
    color: var(--fg);
  }
  .close {
    background: transparent;
    border: none;
    color: var(--fg-muted);
    cursor: pointer;
    font-size: 1.1rem;
    line-height: 1;
    padding: 0;
  }
  .close:hover {
    color: var(--fg);
  }
</style>
