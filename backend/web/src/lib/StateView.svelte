<script lang="ts" generics="T">
  // Shared empty / loading / error wrapper over Resource<T> (D19.13): every
  // zero-data / in-flight / failed surface renders a consistent state instead of
  // an ambiguous blank. Feature lists (logs, functions, roster) render through it.
  import type { Snippet } from 'svelte'
  import type { ResourceView } from './resource.svelte'
  import { m } from '../paraglide/messages.js'

  let {
    resource,
    ready,
    empty,
    isEmpty = (data: T) => Array.isArray(data) && data.length === 0,
    loadingText = m['state.loading'](),
    emptyText = m['state.empty'](),
  }: {
    resource: ResourceView<T>
    /** Rendered with the loaded data when ready and non-empty. */
    ready: Snippet<[T]>
    /** Optional custom empty state; falls back to emptyText. */
    empty?: Snippet
    /** Treats the loaded data as empty (default: an empty array). */
    isEmpty?: (data: T) => boolean
    loadingText?: string
    emptyText?: string
  } = $props()
</script>

{#if resource.status === 'loading' || resource.status === 'idle'}
  <p class="state muted" aria-busy="true">{loadingText}</p>
{:else if resource.status === 'error'}
  <div class="state error" role="alert">
    <p>{resource.error?.message}</p>
    {#if resource.error?.requestId}<p class="muted">{m['app.request']({ id: resource.error.requestId })}</p>{/if}
    <button onclick={resource.reload}>{m['app.retry']()}</button>
  </div>
{:else if resource.data !== null && isEmpty(resource.data)}
  {#if empty}{@render empty()}{:else}<p class="state muted">{emptyText}</p>{/if}
{:else if resource.data !== null}
  {@render ready(resource.data)}
{/if}

<style>
  .state {
    margin: 0;
  }
  .muted {
    color: var(--fg-muted);
  }
  .error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    align-items: flex-start;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  button {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.2rem 0.7rem;
    cursor: pointer;
  }
  button:hover {
    border-color: var(--accent);
  }
</style>
