<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch, toApiError } from '../../lib/api'
  import StateView from '../../lib/StateView.svelte'
  import DevicePicker from '../../lib/DevicePicker.svelte'
  import { session } from '../../lib/auth.svelte'
  import { notify } from '../../lib/toasts.svelte'
  import { mutationAffordance } from '../../lib/readonly'
  import { EventsClient } from '../../lib/events.svelte'
  import {
    type RecentCommand,
    type RecentResponse,
    type CursorMap,
    buildFeedback,
    parseCursorEvent,
    applyCursorEvent,
  } from '../../lib/berry/feedback'
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
  import {
    type Target,
    type RiskLevel,
    buildEnqueue,
    canEnqueue,
    blastRadius,
    riskLevel,
    scriptOk,
    SCRIPT_MAX,
  } from '../../lib/berry/enqueue'
  import type { Device, DevicesResponse } from '../../lib/api/types'
  import { loadDraft, saveDraft } from '../../lib/draft'
  import TemplatePicker from '../../lib/templates/TemplatePicker.svelte'
  import SimulatorPanel from '../../lib/sim/SimulatorPanel.svelte'
  import C2TracePanel from '../../lib/sim/C2TracePanel.svelte'
  import { m } from '../../paraglide/messages.js'

  // Berry C2 command editor (Design 23, W2) — author + lint + autocomplete ONLY. No enqueue ships here
  // (W3) and no feedback (W4): this is the author-safe surface, useful even to a read-only operator as a
  // draft. Every capability/lint/script string renders as a TEXT NODE (Svelte auto-escapes {…}); the raw
  // html directive is banned (D19.10 — catalog docs + script text are operator-influenced).

  const DRAFT_SCOPE = 'berry'
  // Resolved via a closure (not a module const) so the active locale is honored;
  // it is read inside a $effect where a local `m = manifest` shadows the import.
  const starterDoc = (): string => m['berry.starter']()

  // Capability-panel section order; the human heading is resolved per-class below.
  const CLASS_ORDER: CapClass[] = ['command', 'query', 'intent', 'store', 'rtc', 'dev']

  function classTitle(key: CapClass): string {
    switch (key) {
      case 'command':
        return m['berry.class.command']()
      case 'query':
        return m['berry.class.query']()
      case 'intent':
        return m['berry.class.intent']()
      case 'store':
        return m['berry.class.store']()
      case 'rtc':
        return m['berry.class.rtc']()
      case 'dev':
        return m['berry.class.dev']()
      default:
        return key
    }
  }

  const caps = new Resource<CapabilitiesResponse>(() =>
    apiFetch<CapabilitiesResponse>('/api/berry/capabilities'),
  )
  let manifest = $state<Manifest | null>(null)
  let script = $state('')
  let findings = $state<Finding[]>([])
  let editorEl = $state<HTMLDivElement | null>(null)
  let handle: BerryEditorHandle | null = null
  let events: EventsClient | null = null

  void caps.load().then(() => {
    if (caps.data) manifest = { capabilities: caps.data.capabilities, builtins: caps.data.builtins }
  })

  // Mount CM6 once the manifest AND the mount node both exist; guard against a double mount.
  $effect(() => {
    if (!manifest || !editorEl || handle) return
    const m = manifest
    const initial = loadDraft(DRAFT_SCOPE) ?? starterDoc()
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

  onDestroy(() => {
    handle?.destroy()
    events?.close()
  })

  const errorCount = $derived(findings.filter((f) => f.severity === 'error').length)
  const warnCount = $derived(findings.filter((f) => f.severity === 'warning').length)

  function group(m: Manifest, key: CapClass): Capability[] {
    return m.capabilities.filter((c) => c.class === key)
  }

  // ---- enqueue (W3) — the first RCE-shipping surface (Doc 17 §4.5 verbatim, no new backend) ----
  const devices = new Resource<DevicesResponse>(() => apiFetch<DevicesResponse>('/api/devices'))
  let deviceList = $state<Device[]>([])
  let targetKind = $state<'one' | 'fleet'>('one')
  let selectedSerial = $state<string | null>(null)
  let note = $state('')
  let typedArm = $state('') // the operator types '*' here to arm a fleet broadcast (D23.6)
  let enqueuing = $state(false)

  void devices.load().then(() => {
    if (devices.data) deviceList = devices.data.devices
  })

  const target = $derived<Target | null>(
    targetKind === 'fleet'
      ? { kind: 'fleet' }
      : selectedSerial
        ? { kind: 'one', serial: selectedSerial }
        : null,
  )
  // server stays authoritative via requireAdmin (Doc 17 §4.3); this is the cosmetic gate (T7/D19.6).
  const affordance = $derived(mutationAffordance(session.is_admin))
  const risk = $derived<RiskLevel>(riskLevel(findings, target))
  const fleetCount = $derived(blastRadius(deviceList))
  const ready = $derived(canEnqueue(target, typedArm, session.is_admin, script))

  async function doEnqueue(): Promise<void> {
    if (!target || !ready || enqueuing) return
    enqueuing = true
    try {
      const { path, body } = buildEnqueue(target, script, note)
      const res = await apiFetch<{ success: true; seq: number; serial: string }>(path, { method: 'POST', body })
      const where =
        target.kind === 'fleet'
          ? m['berry.where_fleet']({
              devices: fleetCount === 1 ? m['berry.device_one']() : m['berry.device_many']({ n: fleetCount }),
            })
          : target.serial
      notify.success(m['berry.enqueued']({ seq: res.seq, where }))
      typedArm = '' // disarm after a fleet broadcast so the next one re-confirms
      void reloadRecent() // surface the new command in the feedback list (pending until the cursor crosses)
    } catch (e) {
      notify.error(toApiError(e))
    } finally {
      enqueuing = false
    }
  }

  // ---- feedback (W4) — cursor-advance ack, told honestly (D23.4): "applied" = delivered + attempted,
  // NEVER succeeded. Seed from the rehydration read (D23.9), then the c2cursor SSE carries live deltas. ----
  let recentCommands = $state<RecentCommand[]>([])
  let cursors = $state<CursorMap>({})

  async function reloadRecent(): Promise<void> {
    try {
      const res = await apiFetch<RecentResponse>('/api/berry/commands/recent')
      recentCommands = res.commands
    } catch {
      /* feedback is best-effort; a failed rehydration leaves the list empty, not an error page */
    }
  }

  const feedback = $derived(buildFeedback(recentCommands, cursors))

  onMount(() => {
    void reloadRecent()
    events = new EventsClient({
      onC2Cursor: (data) => {
        const ev = parseCursorEvent(data)
        if (ev) cursors = applyCursorEvent(cursors, ev)
      },
    })
    void events.connect()
  })
</script>

<section class="berry">
  <header>
    <h1>{m['berry.title']()}</h1>
    <p class="muted">
      {m['berry.intro']()}
    </p>
  </header>

  <StateView resource={caps} isEmpty={() => false} loadingText={m['berry.loading_caps']()}>
    {#snippet ready()}
      <div class="grid">
        <div class="editor-col">
          <div class="editor" bind:this={editorEl}></div>

          <div class="lint" role="status">
            {#if findings.length === 0}
              <p class="ok">{m['berry.lint_ok']()}</p>
            {:else}
              {@const errText = errorCount === 1 ? m['berry.lint_error_one']() : m['berry.lint_error_many']({ n: errorCount })}
              {@const warnText = warnCount === 1 ? m['berry.lint_warn_one']() : m['berry.lint_warn_many']({ n: warnCount })}
              <p class="lint-summary">{m['berry.lint_summary']({ errors: errText, warnings: warnText })}</p>
              <ul>
                {#each findings as f (f.line + f.kind + f.message)}
                  <li class={f.severity}>
                    <span class="badge">{f.severity === 'error' ? '✗' : '⚠'}</span>
                    <span class="loc">{m['berry.lint_line']({ n: f.line })}</span>
                    <span class="msg">{f.message}</span>
                  </li>
                {/each}
              </ul>
            {/if}
          </div>

          <TemplatePicker
            kind="berry_snippet"
            devices={deviceList}
            onLoad={(src) => handle?.setDoc(src)}
          />

          <div class="enqueue">
            <div class="target" role="radiogroup" aria-label={m['berry.target_aria']()}>
              <span class="lbl">{m['berry.target']()}</span>
              <label><input type="radio" name="target" value="one" bind:group={targetKind} /> {m['berry.target_one']()}</label>
              <label><input type="radio" name="target" value="fleet" bind:group={targetKind} /> {m['berry.target_fleet']()}</label>
            </div>

            {#if targetKind === 'one'}
              <DevicePicker devices={deviceList} bind:value={selectedSerial} placeholder={m['berry.pick_device']()} />
            {:else}
              {@const devices = fleetCount === 1 ? m['berry.device_one']() : m['berry.device_many']({ n: fleetCount })}
              <div class="fleet-arm">
                <p class="danger" role="status">
                  {m['berry.fleet_warn']({ devices })}
                </p>
                <label class="arm">
                  {m['berry.arm_label']()}
                  <input
                    type="text"
                    bind:value={typedArm}
                    aria-label={m['berry.arm_aria']()}
                    autocomplete="off"
                  />
                </label>
              </div>
            {/if}

            {#if risk !== 'normal'}
              <p class="risk {risk}" role="alert">
                {#if risk === 'fleet-severing'}
                  {m['berry.risk_fleet_severing']()}
                {:else}
                  {m['berry.risk_severing']()}
                {/if}
              </p>
            {/if}

            <label class="note">
              {m['berry.note']()}
              <input
                type="text"
                bind:value={note}
                placeholder={m['berry.note_ph']()}
                maxlength="200"
              />
            </label>

            <div class="actions">
              <button
                type="button"
                class="enqueue-btn"
                disabled={affordance.disabled || !ready || enqueuing}
                title={affordance.disabled ? affordance.title : ''}
                aria-disabled={affordance['aria-disabled'] || !ready}
                onclick={doEnqueue}
              >
                {enqueuing ? m['berry.enqueuing']() : m['berry.enqueue']()}
              </button>
              {#if !scriptOk(script)}
                <span class="muted">{m['berry.chars']({ len: script.length, max: SCRIPT_MAX })}</span>
              {/if}
              {#if errorCount > 0}
                {@const errText = errorCount === 1 ? m['berry.lint_error_one']() : m['berry.lint_error_many']({ n: errorCount })}
                <span class="warn-inline">{m['berry.lint_error_inline']({ count: errText })}</span>
              {/if}
            </div>
          </div>
        </div>

        {#if manifest}
          <aside class="caps">
            <h2>{m['berry.caps_heading']()}</h2>
            <p class="muted">{m['berry.caps_sub']()}</p>
            {#each CLASS_ORDER as key (key)}
              {@const list = group(manifest, key)}
              {#if list.length > 0}
                <h3>{classTitle(key)}</h3>
                <ul class="caplist">
                  {#each list as c (c.name)}
                    <li>
                      <code>{signature(c)}</code>
                      {#if c.risk === RISK_SEVERING}<span class="sev" title={m['berry.severing_title']()}>{m['berry.sev_severing']()}</span>{/if}
                      <span class="doc">{c.doc}</span>
                    </li>
                  {/each}
                </ul>
              {/if}
            {/each}
          </aside>
        {/if}
      </div>

      <C2TracePanel {script} {manifest} />

      <SimulatorPanel />

      <section class="feedback">
        <h2>{m['berry.feedback']()}</h2>
        <p class="muted">
          {m['berry.feedback_note']()}
        </p>
        {#if feedback.length === 0}
          <p class="muted">{m['berry.no_recent']()}</p>
        {:else}
          <ul class="fb-list">
            {#each feedback as f (f.seq)}
              <li class="fb {f.state}">
                <span class="seq">{m['berry.seq']({ n: f.seq })}</span>
                <span class="fb-serial mono">{f.serial}</span>
                <span class="fb-label">{f.label}</span>
                {#if f.note}<span class="fb-note">“{f.note}”</span>{/if}
                {#if f.logSerial}<a class="loglink" href="/logs" title={m['berry.view_log_title']({ serial: f.logSerial })}>{m['berry.view_log']()}</a>{/if}
              </li>
            {/each}
          </ul>
        {/if}
      </section>
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
  .enqueue {
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.6rem 0.75rem;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }
  .target {
    display: flex;
    align-items: center;
    gap: 1rem;
    flex-wrap: wrap;
  }
  .target .lbl {
    color: var(--fg-muted);
    text-transform: uppercase;
    font-size: 0.75rem;
    letter-spacing: 0.04em;
  }
  .target label {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
  }
  .fleet-arm {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .danger {
    margin: 0;
    color: var(--danger);
    font-size: 0.85rem;
  }
  .arm {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
  }
  .arm input,
  .note input {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.3rem 0.5rem;
    font-size: 0.88rem;
  }
  .arm input {
    width: 4rem;
    font-family: var(--mono, monospace);
  }
  .note {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.82rem;
    color: var(--fg-muted);
  }
  .risk {
    margin: 0;
    border-radius: 6px;
    padding: 0.4rem 0.6rem;
    font-size: 0.85rem;
  }
  .risk.severing {
    border: 1px solid var(--warn, #d08770);
    color: var(--warn, #d08770);
  }
  .risk.fleet-severing {
    border: 1px solid var(--danger);
    color: var(--danger);
    font-weight: 600;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .enqueue-btn {
    background: var(--accent);
    border: 1px solid var(--accent);
    color: var(--bg, #0b0f17);
    border-radius: 6px;
    padding: 0.35rem 1.1rem;
    cursor: pointer;
    font-weight: 600;
  }
  .enqueue-btn:disabled {
    background: transparent;
    color: var(--fg-muted);
    cursor: not-allowed;
    border-color: var(--border);
  }
  .warn-inline {
    color: var(--warn, #d08770);
    font-size: 0.8rem;
  }
  .feedback {
    border-top: 1px solid var(--border);
    padding-top: 0.75rem;
  }
  .feedback h2 {
    margin: 0 0 0.25rem;
    font-size: 1rem;
  }
  .fb-list {
    list-style: none;
    margin: 0.5rem 0 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .fb {
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
    flex-wrap: wrap;
    border-left: 3px solid var(--border);
    padding: 0.2rem 0.6rem;
    font-size: 0.85rem;
  }
  .fb.applied {
    border-left-color: var(--accent);
  }
  .fb.partial {
    border-left-color: var(--warn, #d08770);
  }
  .fb.pending {
    border-left-color: var(--fg-muted);
  }
  .fb .seq {
    color: var(--fg-muted);
    font-variant-numeric: tabular-nums;
  }
  .fb-serial {
    font-weight: 600;
  }
  .fb-note {
    color: var(--fg-muted);
    font-style: italic;
  }
  .loglink {
    color: var(--accent);
    font-size: 0.8rem;
  }
</style>
