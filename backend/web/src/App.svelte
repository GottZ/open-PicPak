<script lang="ts">
  // WAVE 2 SCAFFOLD: a minimal mount target so the bun → vite → //go:embed →
  // Go-serve path is provable end-to-end (toolchain + Dockerfile stage).
  // Wave 3 replaces this with the router + login gate + admin/read-only shell.
</script>

<main>
  <h1>open-picpak admin</h1>
  <p>Frontend shell bootstrapping — wave 3 mounts the router here.</p>
</main>

<style>
  main {
    max-width: 40rem;
    margin: 4rem auto;
    padding: 0 1rem;
  }
  h1 {
    font-size: 1.5rem;
    font-weight: 600;
  }
  p {
    color: var(--fg-muted);
  }
</style>
