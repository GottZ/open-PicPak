<script lang="ts">
  import { onMount } from 'svelte'
  import { Router, isActiveLink } from 'sv-router'
  import './router' // side effect: instantiate createRouter before <Router/> mounts
  import { session } from './lib/auth.svelte'
  import { AREAS } from './routes'
  import Login from './Login.svelte'

  onMount(() => void session.restore())
</script>

{#if session.restoring}
  <div class="boot" aria-busy="true">
    <p>restoring session…</p>
  </div>
{:else if !session.active}
  <Login />
{:else}
  <div class="shell">
    <header class="topbar">
      <span class="brand">open-picpak</span>
      <nav>
        {#each AREAS as a (a.path)}
          <a href={a.path} {@attach isActiveLink()}>{a.title}</a>
        {/each}
      </nav>
      <div class="identity">
        <!-- Read-only degradation (D19.6): the badge is comfort; the server's
             requireAdmin (design 17 §4.2) is the truth. -->
        <span class="badge" class:admin={session.is_admin}>
          {session.is_admin ? 'admin' : 'read-only'}
        </span>
        <span class="key-label">{session.label || `key #${session.keyId}`}</span>
        <button class="logout" onclick={() => session.logout()}>sign out</button>
      </div>
    </header>
    <main>
      <Router />
    </main>
  </div>
{/if}

<style>
  .boot {
    min-height: 100dvh;
    display: grid;
    place-content: center;
    color: var(--fg-muted);
    font-family: monospace;
  }
  .shell {
    min-height: 100dvh;
    display: flex;
    flex-direction: column;
  }
  .topbar {
    display: flex;
    align-items: center;
    gap: 1.5rem;
    padding: 0.6rem 1.25rem;
    border-bottom: 1px solid var(--border);
    background: var(--surface);
  }
  .brand {
    font-weight: 700;
    letter-spacing: -0.01em;
  }
  nav {
    display: flex;
    gap: 0.25rem;
    flex: 1;
  }
  nav a {
    color: var(--fg-muted);
    text-decoration: none;
    padding: 0.3rem 0.7rem;
    border-radius: 6px;
    font-size: 0.9rem;
  }
  nav a:hover {
    color: var(--fg);
    background: rgba(255, 255, 255, 0.04);
  }
  nav a:global(.is-active) {
    color: var(--fg);
    background: rgba(122, 162, 247, 0.14);
  }
  .identity {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    font-size: 0.85rem;
  }
  .badge {
    font-family: monospace;
    font-size: 0.7rem;
    letter-spacing: 0.06em;
    text-transform: uppercase;
    padding: 0.1rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--warn);
    color: var(--warn);
  }
  .badge.admin {
    border-color: var(--ok);
    color: var(--ok);
  }
  .key-label {
    color: var(--fg-muted);
    max-width: 12rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .logout {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-muted);
    border-radius: 6px;
    padding: 0.2rem 0.6rem;
    cursor: pointer;
    font-size: 0.8rem;
  }
  .logout:hover {
    color: var(--fg);
    border-color: var(--accent);
  }
  main {
    flex: 1;
    padding: 1.5rem;
  }
</style>
