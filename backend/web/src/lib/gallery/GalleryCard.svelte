<script lang="ts">
  // One template card in the Vorlagen gallery (design/33 §4.5b, W-A33.5b). A render_fn card shows a
  // rendered preview (the W-A33.5a route; until that backend wave lands the <img> degrades to a neutral
  // "no preview" note via onerror); a berry_snippet card shows a plain-language effect summary derived from
  // the STATIC source (summary.ts → effect-text.ts) and a "Was macht das?" toggle that opens the
  // illustrative C2 trace (S8-labelled inside that panel). Every string is a TEXT NODE — no @html directive
  // (S7); name/description/effect are template-authored and operator-influenced.
  import { onMount } from 'svelte'
  import { m } from '../../paraglide/messages.js'
  import { activeLocale, localizedDescription } from '../i18n'
  import { toApiError } from '../api'
  import type { Manifest } from '../berry/catalog'
  import type { EffectDescription } from '../sim/effect-text'
  import { localizeEffect } from '../sim/effect-msg'
  import C2TracePanel from '../sim/C2TracePanel.svelte'
  import { getTemplate } from '../templates/client'
  import type { TemplateSummary } from '../templates/types'
  import { effectSummary } from './summary'

  let { template, manifest, onUse }: {
    template: TemplateSummary
    manifest: Manifest | null
    onUse: (t: TemplateSummary) => void
  } = $props()

  const isSnippet = $derived(template.kind === 'berry_snippet')
  const isRender = $derived(template.kind === 'render_fn')
  // The card name drops the reserved 'builtin/' prefix — a laie reads "weather", not "builtin/weather".
  const displayName = $derived(template.name.replace(/^builtin\//, ''))
  // The editor deep-link for "Anpassen": render_fn → /functions, berry_snippet → /berry, each with the
  // ?template= hint. sv-router intercepts the same-origin anchor (the shell nav uses the same idiom).
  const editHref = $derived(`${isRender ? '/functions' : '/berry'}?template=${template.id}`)
  const previewUrl = $derived(`/api/templates/${template.id}/preview?v=${template.version}`)

  let previewBroken = $state(false)
  let source = $state<string | null>(null)
  let sourceLoading = $state(false)
  let showTrace = $state(false)

  const summary = $derived<EffectDescription[]>(
    isSnippet && source !== null && manifest !== null ? effectSummary(source, manifest) : [],
  )

  onMount(() => {
    // Snippet cards need the source for the effect summary + the trace; render_fn cards do not.
    if (isSnippet) void loadSource()
  })

  async function loadSource(): Promise<void> {
    if (source !== null || sourceLoading) return
    sourceLoading = true
    try {
      const t = await getTemplate(template.id)
      source = t.source
    } catch {
      source = '' // a failed load leaves an empty summary, not an error card
    } finally {
      sourceLoading = false
    }
  }

  const description = $derived(localizedDescription(template.description, activeLocale()))
</script>

<article class="card">
  <div class="preview">
    {#if isRender && !previewBroken}
      <img src={previewUrl} alt={m['gallery.card.preview_alt']({ name: displayName })} onerror={() => (previewBroken = true)} loading="lazy" />
    {:else if isRender}
      <div class="no-preview muted">{m['gallery.card.no_preview']()}</div>
    {:else}
      <div class="kind-glyph" aria-hidden="true">⌘</div>
    {/if}
  </div>

  <div class="body">
    <div class="titlerow">
      <span class="name">{displayName}</span>
      {#if template.builtin}<span class="badge builtin">{m['templates.builtin_badge']()}</span>{/if}
      <span class="badge kind">{isRender ? m['gallery.card.kind_render']() : m['gallery.card.kind_snippet']()}</span>
    </div>

    {#if description}
      <p class="desc muted">{description}</p>
    {/if}

    {#if isSnippet}
      <div class="effect">
        <p class="effect-head">{m['gallery.card.effect_heading']()}</p>
        {#if sourceLoading}
          <p class="muted small" aria-busy="true">{m['gallery.apply.summary_loading']()}</p>
        {:else if summary.length === 0}
          <p class="muted small">{m['gallery.apply.no_effect']()}</p>
        {:else}
          <ul class="effect-list">
            {#each summary as d, i (i)}
              <li class:severing={d.severing}>{localizeEffect(d)}</li>
            {/each}
          </ul>
        {/if}
      </div>
    {/if}

    <div class="actions">
      <button type="button" class="primary" onclick={() => onUse(template)}>{m['gallery.card.use']()}</button>
      <a class="btn" href={editHref}>{m['gallery.card.customize']()}</a>
      {#if isSnippet}
        <button type="button" class="btn" aria-expanded={showTrace} onclick={() => (showTrace = !showTrace)}>
          {m['gallery.card.explain']()}
        </button>
      {/if}
    </div>

    {#if isSnippet && showTrace && source}
      <div class="trace-wrap">
        <C2TracePanel script={source} {manifest} />
      </div>
    {/if}
  </div>
</article>

<style>
  .card {
    display: flex;
    flex-direction: column;
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
    background: var(--bg-elev, var(--bg));
  }
  .preview {
    aspect-ratio: 4 / 3;
    background: var(--bg);
    border-bottom: 1px solid var(--border);
    display: flex;
    align-items: center;
    justify-content: center;
    overflow: hidden;
  }
  .preview img {
    width: 100%;
    height: 100%;
    object-fit: contain;
  }
  .no-preview {
    font-size: 0.8rem;
  }
  .kind-glyph {
    font-size: 2rem;
    opacity: 0.35;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
    padding: 0.75rem;
  }
  .titlerow {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .name {
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.9rem;
    font-weight: 600;
  }
  .badge {
    font-size: 0.68rem;
    padding: 0.05rem 0.4rem;
    border: 1px solid var(--border);
    border-radius: 999px;
    white-space: nowrap;
  }
  .desc {
    margin: 0;
    font-size: 0.82rem;
  }
  .muted {
    color: var(--fg-muted);
  }
  .small {
    font-size: 0.78rem;
  }
  .effect-head {
    margin: 0 0 0.2rem;
    font-size: 0.78rem;
    font-weight: 600;
  }
  .effect-list {
    list-style: disc;
    margin: 0;
    padding-left: 1.1rem;
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    font-size: 0.8rem;
  }
  .effect-list li.severing {
    color: var(--warn, #d08770);
  }
  .actions {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
    margin-top: 0.15rem;
  }
  .btn,
  button {
    font-size: 0.82rem;
    padding: 0.3rem 0.6rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: transparent;
    color: var(--fg);
    cursor: pointer;
    text-decoration: none;
  }
  .primary {
    background: var(--accent, #7aa2f7);
    color: var(--accent-fg, #10131c);
    border-color: transparent;
    font-weight: 600;
  }
  .trace-wrap {
    margin-top: 0.3rem;
  }
</style>
