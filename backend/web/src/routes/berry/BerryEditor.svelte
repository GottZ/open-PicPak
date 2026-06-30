<script lang="ts">
  import { onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import StateView from '../../lib/StateView.svelte'
  import { createBerryEditor, type BerryEditorHandle } from '../../lib/berry/editor'
  import { lint, type Finding } from '../../lib/berry/lint'
  import {
    type Manifest,
    type Capability,
    type CapabilitiesResponse,
    type CapClass,
    signature,
    RISK_SEVERING,
  } from '../../lib/berry/catalog'
  import { loadDraft, saveDraft } from '../../lib/draft'

  // Berry C2 command editor (Design 23, W2) — author + lint + autocomplete ONLY. No enqueue ships here
  // (W3) and no feedback (W4): this is the author-safe surface, useful even to a read-only operator as a
  // draft. Every capability/lint/script string renders as a TEXT NODE (Svelte auto-escapes {…}); the raw
  // html directive is banned (D19.10 — catalog docs + script text are operator-influenced).

  const DRAFT_SCOPE = 'berry'
  const STARTER = '# Author a Berry C2 command. Autocomplete offers the safe subset only.\n'

  // class → human heading + order for the capability panel.
  const CLASS_ORDER: { key: CapClass; title: string }[] = [
    { key: 'command', title: 'config / writes' },
    { key: 'query', title: 'queries' },
    { key: 'intent', title: 'control-flow intents' },
    { key: 'store', title: 'store (flash kv)' },
    { key: 'rtc', title: 'rtc memory' },
    { key: 'dev', title: 'device (read-only)' },
  ]

  const caps = new Resource<CapabilitiesResponse>(() =>
    apiFetch<CapabilitiesResponse>('/api/berry/capabilities'),
  )
  let manifest = $state<Manifest | null>(null)
  let script = $state('')
  let findings = $state<Finding[]>([])
  let editorEl = $state<HTMLDivElement | null>(null)
  let handle: BerryEditorHandle | null = null

  void caps.load().then(() => {
    if (caps.data) manifest = { capabilities: caps.data.capabilities, builtins: caps.data.builtins }
  })

  // Mount CM6 once the manifest AND the mount node both exist; guard against a double mount.
  $effect(() => {
    if (!manifest || !editorEl || handle) return
    const m = manifest
    const initial = loadDraft(DRAFT_SCOPE) ?? STARTER
    script = initial
    findings = lint(initial, m)
    handle = createBerryEditor({
      parent: editorEl,
      doc: initial,
      manifest: m,
      onChange: (doc) => {
        script = doc
        saveDraft(DRAFT_SCOPE, doc)
        findings = lint(doc, m)
      },
    })
  })

  onDestroy(() => handle?.destroy())

  const errorCount = $derived(findings.filter((f) => f.severity === 'error').length)
  const warnCount = $derived(findings.filter((f) => f.severity === 'warning').length)

  function group(m: Manifest, key: CapClass): Capability[] {
    return m.capabilities.filter((c) => c.class === key)
  }
</script>

<section class="berry">
  <header>
    <h1>Berry</h1>
    <p class="muted">
      Author a C2 command script. Lint and autocomplete are best-effort, client-side, pre-flight — the
      authoritative compile check is on-device, and its result never returns (the cursor advances either
      way). Enqueue ships in a later wave.
    </p>
  </header>

  <StateView resource={caps} isEmpty={() => false} loadingText="loading capabilities…">
    {#snippet ready()}
      <div class="grid">
        <div class="editor-col">
          <div class="editor" bind:this={editorEl}></div>

          <div class="lint" role="status">
            {#if findings.length === 0}
              <p class="ok">✓ no structural issues</p>
            {:else}
              <p class="lint-summary">
                lint: {errorCount} error{errorCount === 1 ? '' : 's'} · {warnCount} warning{warnCount ===
                1
                  ? ''
                  : 's'}
              </p>
              <ul>
                {#each findings as f (f.line + f.kind + f.message)}
                  <li class={f.severity}>
                    <span class="badge">{f.severity === 'error' ? '✗' : '⚠'}</span>
                    <span class="loc">line {f.line}</span>
                    <span class="msg">{f.message}</span>
                  </li>
                {/each}
              </ul>
            {/if}
          </div>
        </div>

        {#if manifest}
          <aside class="caps">
            <h2>capabilities</h2>
            <p class="muted">the C2 safe subset — autocomplete offers exactly these.</p>
            {#each CLASS_ORDER as section (section.key)}
              {@const list = group(manifest, section.key)}
              {#if list.length > 0}
                <h3>{section.title}</h3>
                <ul class="caplist">
                  {#each list as c (c.name)}
                    <li>
                      <code>{signature(c)}</code>
                      {#if c.risk === RISK_SEVERING}<span class="sev" title="can cut the device's own C2 path">⚠ severing</span>{/if}
                      <span class="doc">{c.doc}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            {/each}
          </aside>
        {/if}
      </div>
    {/snippet}
  </StateView>
</section>

<style>
  .berry {
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
  .grid {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
    gap: 1rem;
    align-items: start;
  }
  .editor-col {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    min-width: 0;
  }
  .lint {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
  }
  .lint ul,
  .caplist {
    list-style: none;
    margin: 0.3rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .lint li {
    display: flex;
    gap: 0.5rem;
    align-items: baseline;
  }
  .lint li.error .badge,
  .lint li.error .msg {
    color: var(--danger);
  }
  .lint li.warning .badge {
    color: var(--warn, #d08770);
  }
  .ok {
    color: var(--ok, #4caf50);
    margin: 0;
  }
  .lint-summary {
    margin: 0;
    font-weight: 600;
  }
  .loc {
    color: var(--fg-muted);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .caps {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
  }
  .caps h2 {
    margin: 0 0 0.25rem;
    font-size: 1rem;
  }
  .caps h3 {
    margin: 0.75rem 0 0.25rem;
    font-size: 0.8rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-muted);
  }
  .caplist li {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }
  .caplist code {
    font-family: var(--mono, ui-monospace, monospace);
  }
  .sev {
    color: var(--warn, #d08770);
    font-size: 0.75rem;
  }
  .doc {
    color: var(--fg-muted);
    font-size: 0.8rem;
  }
</style>
