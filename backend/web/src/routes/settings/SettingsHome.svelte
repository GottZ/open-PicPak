<script lang="ts">
  // /settings area shell (design 02-settings-spa §4/§5 B5/§8 E4, W2) — replaces the
  // AreaPlaceholder slot for '/settings' (routes/index.ts). Unlike every other
  // operator area, JEDE Secrets-Route inkl. LIST ist admin-gated (secrets_api.go,
  // backend.go:178-182) — a non-admin's GET /api/secrets 403s. Rendering that 403
  // as an empty Resource error row would read as "no secrets" (B5). This shell is
  // the fail-closed fix: it holds the admin-gate state and mounts SecretsPanel
  // ONLY for a confirmed admin; a confirmed non-admin gets an explicit
  // "needs admin" state, never a bare/empty table.
  //
  // RESTORE-ORDNUNG (§5 B5 restoring-Unterscheidung, §8 E4): session.is_admin is
  // `$derived(this.whoami?.is_admin ?? false)` (auth.svelte.ts:22) and falls back
  // to false WHILE the boot-time whoami probe is in flight (auth.svelte.ts:15
  // `restoring`, :43 `restore()`) — evaluating `!session.is_admin` alone would
  // flash "needs admin" at a legitimate admin on every reload until whoami
  // resolves. The gate below is REACTIVE (a template {#if} bound directly to the
  // $derived fields, never a one-shot read like FunctionsEditor.svelte:61's
  // `if (session.is_admin)` at module scope): while `session.restoring` it renders
  // a neutral idle state; SecretsPanel — and therefore its LIST-load — mounts only
  // AFTER whoami has resolved, so a non-admin→admin transition self-heals without
  // ever having rendered the locked state to begin with.
  import { session } from '../../lib/auth.svelte'
  import SecretsPanel from './SecretsPanel.svelte'
  import { m } from '../../paraglide/messages.js'
</script>

<section class="settings">
  <header>
    <h1>{m['settings.title']()}</h1>
    <p class="subtitle">{m['settings.subtitle']()}</p>
  </header>

  {#if session.restoring}
    <p class="state muted" aria-busy="true">{m['app.restoring']()}</p>
  {:else if session.is_admin}
    <SecretsPanel />
  {:else}
    <p class="state needs-admin" role="alert">{m['settings.needs_admin']()}</p>
  {/if}
</section>

<style>
  .settings {
    display: flex;
    flex-direction: column;
    gap: 1.5rem;
    max-width: 60rem;
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.5rem;
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
  }
  .subtitle {
    margin: 0.35rem 0 0;
    color: var(--fg-muted);
    font-size: 0.875rem;
  }
  .state {
    margin: 0;
  }
  .muted {
    color: var(--fg-muted);
  }
  .needs-admin {
    border: 1px solid var(--warn, #d08770);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    color: var(--warn, #d08770);
  }
</style>
