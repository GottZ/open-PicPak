<script lang="ts">
  // Berry-WASM render-profile simulator panel (design/33 §4.3/§4.4). Self-contained: it owns its own
  // BerrySim Worker (E-A33-2 resolved to a Web Worker in W-A33.2, so the VM runs off the UI thread and the
  // 100 ms deadline is a Worker-local budget — no main-thread jank is possible; the measurement mandate is
  // moot for the built infra). It hosts a render-script editor (createSimEditor — NOT the C2 command
  // editor, since fb.* is `forbidden` there), scenario presets, an example dropdown, a raw-values panel,
  // the FramePreview output, a permanent boundary declaration (S5/S8), and the sim-manifest pin (W11).
  //
  // The WASM (~80 KB .wasm.br) is a build product loaded lazily and off the initial bundle. It is idle-
  // imported after first paint; without it the panel names an "unavailable" state and the editor stays
  // fully functional (§7 probe b) — fail-open, on-device authoritative. Every VM / manifest string renders
  // as a TEXT NODE (Svelte auto-escapes {…}); {@html} is banned (D19.10).
  import { onMount, onDestroy } from 'svelte'
  import { m } from '../../paraglide/messages.js'
  import FramePreview from '../media/FramePreview.svelte'
  import { loadDraft, saveDraft } from '../draft'
  import { createSimEditor, type SimEditorHandle, type VmCheck } from '../berry/editor'
  import { BerrySim, type SimResult } from './loader'
  import { SCENARIOS, DEFAULT_SCENARIO_ID, scenarioById, devJson, type DevState } from './scenarios'
  import { SIM_EXAMPLES, STARTER_SOURCE, exampleById } from './examples'
  import { vmSyntaxFinding } from './diagnostics'
  import { pinLine, type SimManifest } from './manifest'

  const DRAFT_SCOPE = 'sim'
  const MANIFEST_URL = '/picpak-berry.manifest.json'

  // Explicit id → label maps (no dynamic `m` key — svelte-check rejects computed message access; same
  // discipline as TemplatePicker's APPLY_ERR_MSG).
  const SCENARIO_LABEL: Record<string, () => string> = {
    normal: () => m['sim.scenario.normal'](),
    fresh: () => m['sim.scenario.fresh'](),
    low: () => m['sim.scenario.low'](),
    aged: () => m['sim.scenario.aged'](),
  }
  const EXAMPLE_LABEL: Record<string, () => string> = {
    starter: () => m['sim.example.starter'](),
    render16: () => m['sim.example.render16'](),
    render_qr: () => m['sim.example.render_qr'](),
  }

  type SimStatus = 'loading' | 'ready' | 'unavailable'

  let editorEl = $state<HTMLDivElement | null>(null)
  let handle: SimEditorHandle | null = null
  let sim: BerrySim | null = null
  let simStatus = $state<SimStatus>('loading')

  let script = loadDraft(DRAFT_SCOPE) ?? STARTER_SOURCE
  let scenarioId = $state(DEFAULT_SCENARIO_ID)
  let dev = $state<DevState>({ ...scenarioById(DEFAULT_SCENARIO_ID).dev })
  let rawMode = $state(false)
  let exampleChoice = $state('')

  let frame = $state<Uint8Array | null>(null)
  let running = $state(false)
  let runError = $state<string | null>(null)

  let pinText = $state<string | null>(null)

  // The VM syntax check the render editor's linter calls. Returns null (⇒ no squiggle) until the WASM is
  // ready and whenever the simulator is unavailable — fail-open (§4.4).
  const vmCheck: VmCheck = async (doc: string) => {
    if (!sim || simStatus !== 'ready') return null
    const r = await sim.compileOnly(doc)
    return vmSyntaxFinding(r.rc, r.error)
  }

  // Mount the editor once its node exists — independent of the WASM, so the editor works with or without
  // the simulator (probe b). Guard against a double mount.
  $effect(() => {
    if (!editorEl || handle) return
    handle = createSimEditor({
      parent: editorEl,
      doc: script,
      vmCheck,
      onChange: (doc) => {
        script = doc
        saveDraft(DRAFT_SCOPE, doc)
      },
    })
  })

  onMount(() => {
    void loadManifest()
    // Idle-load the VM after first paint so the panel opens instantly (§4.4).
    const idle =
      typeof requestIdleCallback === 'function'
        ? requestIdleCallback
        : (cb: () => void) => setTimeout(cb, 200)
    idle(() => void loadSim())
  })

  onDestroy(() => {
    handle?.destroy()
    sim?.dispose()
  })

  async function loadManifest(): Promise<void> {
    try {
      const res = await fetch(MANIFEST_URL)
      if (!res.ok) return
      pinText = pinLine((await res.json()) as SimManifest)
    } catch {
      /* unbuilt in dev — the panel shows the "built at deploy" note instead */
    }
  }

  async function loadSim(): Promise<void> {
    simStatus = 'loading'
    try {
      const s = new BerrySim()
      // Probe availability: a trivial compile fails with rc<0 only when the module never loaded.
      const probe = await s.compileOnly('')
      if (probe.rc < 0) {
        s.dispose()
        simStatus = 'unavailable'
        return
      }
      sim = s
      simStatus = 'ready'
      handle?.refreshLint() // re-check existing content now that the VM is live
    } catch {
      simStatus = 'unavailable'
    }
  }

  function onScenarioChange(): void {
    dev = { ...scenarioById(scenarioId).dev }
    // Auto-render so the preset visibly moves the frame (probe c) once the sim is live and a frame exists.
    if (simStatus === 'ready' && frame) void render()
  }

  function onLoadExample(): void {
    const ex = exampleById(exampleChoice)
    if (!ex) return
    handle?.setDoc(ex.source)
    script = ex.source
    saveDraft(DRAFT_SCOPE, ex.source)
    frame = null
    runError = null
    handle?.refreshLint()
    exampleChoice = '' // reset so the same example can be re-loaded
  }

  function runErrorText(r: SimResult): string {
    if (r.rc === 1) return `${m['sim.error_compile']()}: ${r.error}`
    if (r.rc === 3) return m['sim.error_deadline']()
    if (r.rc < 0) return m['sim.error_unavailable']()
    return `${m['sim.error_runtime']()}: ${r.error}`
  }

  async function render(): Promise<void> {
    if (!sim || simStatus !== 'ready' || running) return
    running = true
    runError = null
    try {
      const r = await sim.run(script, devJson(dev))
      if (r.rc === 0 && r.fb) {
        frame = r.fb
      } else {
        runError = runErrorText(r)
      }
    } finally {
      running = false
    }
  }
</script>

<section class="sim">
  <header>
    <h2>{m['sim.heading']()}</h2>
    <p class="muted">{m['sim.subtitle']()}</p>
  </header>

  {#if simStatus === 'unavailable'}
    <div class="unavailable" role="status">
      <strong>{m['sim.unavailable_heading']()}</strong>
      <p class="muted">{m['sim.unavailable_body']()}</p>
    </div>
  {/if}

  <div class="controls">
    <label class="ctl">
      <span>{m['sim.example_label']()}</span>
      <select bind:value={exampleChoice} onchange={onLoadExample} aria-label={m['sim.example_label']()}>
        <option value="" disabled selected>{m['sim.example_placeholder']()}</option>
        {#each SIM_EXAMPLES as ex (ex.id)}
          <option value={ex.id}>{EXAMPLE_LABEL[ex.id]?.() ?? ex.id}</option>
        {/each}
      </select>
    </label>

    <label class="ctl">
      <span>{m['sim.scenario_label']()}</span>
      <select bind:value={scenarioId} onchange={onScenarioChange} aria-label={m['sim.scenario_label']()}>
        {#each SCENARIOS as sc (sc.id)}
          <option value={sc.id}>{SCENARIO_LABEL[sc.id]?.() ?? sc.id}</option>
        {/each}
      </select>
    </label>

    <button
      type="button"
      class="run-btn"
      disabled={simStatus !== 'ready' || running}
      onclick={render}
    >
      {running ? m['sim.running']() : m['sim.run']()}
    </button>
  </div>

  <div class="editor" bind:this={editorEl}></div>

  <details class="raw" bind:open={rawMode}>
    <summary>{m['sim.raw_summary']()}</summary>
    <div class="raw-grid">
      <label>
        <span>{m['sim.raw.batt_mv']()}</span>
        <input type="number" bind:value={dev.batt_mv} />
      </label>
      <label>
        <span>{m['sim.raw.batt_pct']()}</span>
        <input type="number" bind:value={dev.batt_pct} />
      </label>
      <label>
        <span>{m['sim.raw.uptime_ms']()}</span>
        <input type="number" bind:value={dev.uptime_ms} />
      </label>
    </div>
  </details>

  <div class="output">
    {#if runError}
      <p class="run-error" role="alert">{runError}</p>
    {/if}
    {#if frame}
      <FramePreview packed={frame} alt={m['sim.frame_alt']()} />
    {:else}
      <p class="muted no-frame">{m['sim.no_frame']()}</p>
    {/if}
  </div>

  <div class="boundary" role="note">
    <strong>{m['sim.boundary_heading']()}</strong>
    <p class="muted">{m['sim.boundary_body']()}</p>
    <p class="pin muted">{pinText ? `${m['sim.pin_prefix']()} ${pinText}` : m['sim.pin_unbuilt']()}</p>
  </div>
</section>

<style>
  .sim {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.75rem 1rem;
  }
  header h2 {
    margin: 0 0 0.25rem;
    font-size: 1rem;
  }
  .muted {
    color: var(--fg-muted);
    margin: 0;
  }
  .unavailable {
    border: 1px solid var(--warn, #d08770);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .controls {
    display: flex;
    gap: 1rem;
    align-items: flex-end;
    flex-wrap: wrap;
  }
  .ctl {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
  }
  .ctl select {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.3rem 0.5rem;
    font-size: 0.88rem;
  }
  .run-btn {
    background: var(--accent);
    border: 1px solid var(--accent);
    color: var(--bg, #0b0f17);
    border-radius: 6px;
    padding: 0.35rem 1.1rem;
    cursor: pointer;
    font-weight: 600;
  }
  .run-btn:disabled {
    background: transparent;
    color: var(--fg-muted);
    cursor: not-allowed;
    border-color: var(--border);
  }
  .raw summary {
    cursor: pointer;
    font-size: 0.82rem;
    color: var(--fg-muted);
  }
  .raw-grid {
    display: flex;
    gap: 0.75rem;
    flex-wrap: wrap;
    margin-top: 0.5rem;
  }
  .raw-grid label {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.78rem;
    color: var(--fg-muted);
  }
  .raw-grid input {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.3rem 0.5rem;
    width: 8rem;
    font-size: 0.85rem;
  }
  .run-error {
    color: var(--danger);
    font-size: 0.85rem;
    margin: 0 0 0.5rem;
    white-space: pre-wrap;
  }
  .no-frame {
    font-size: 0.85rem;
  }
  .boundary {
    border-top: 1px solid var(--border);
    padding-top: 0.6rem;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
    font-size: 0.82rem;
  }
  .pin {
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.75rem;
  }
</style>
