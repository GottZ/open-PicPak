<script lang="ts">
  // C2 effect-trace panel (design/33 §4.3/§7, W-A33.4). ILLUSTRATIVE view: it runs the current C2 command
  // script against the pure-TS 36-stub surface (c2-trace.ts) under a chosen device state and lists the
  // stub calls that actually ran, each translated to a human effect (effect-text.ts). The device state
  // selector makes S8 tangible — flipping "online" → "offline" makes `if !connected() net_clear() end`
  // trace net_clear, proving the trace depends on data.
  //
  // S8 — THE HARD LABEL: severing warnings are NOT drawn from this trace. They come from the static
  // lint.ts calls() pass shown above the editor (branch-independent). This panel says so permanently
  // ("Statik schlägt Trace"): a call hidden behind a sim-false branch can be on-device-true. Every
  // string is a TEXT NODE (Svelte auto-escapes {…}); {@html} is banned (D19.10 — script + catalog text
  // are operator-influenced).
  import { m } from '../../paraglide/messages.js'
  import type { Manifest } from '../berry/catalog'
  import { traceC2 } from './c2-trace'
  import type { Arg } from './c2-surface'
  import { C2_SCENARIOS, DEFAULT_C2_SCENARIO_ID, c2ScenarioById, formatArgs, formatArg } from './c2-surface'
  import { describeEffect, type EffectKey } from './effect-text'

  interface Props {
    script: string
    manifest: Manifest | null
  }
  let { script, manifest }: Props = $props()

  // Explicit id → label / message maps: svelte-check rejects a computed `m[...]` key (same discipline as
  // SimulatorPanel's SCENARIO_LABEL).
  const SCENARIO_LABEL: Record<string, () => string> = {
    online: () => m['sim.c2.online'](),
    offline: () => m['sim.c2.offline'](),
    night: () => m['sim.c2.night'](),
    unprovisioned: () => m['sim.c2.unprovisioned'](),
  }
  // Params are extracted per key (paraglide generates a strict Inputs type per message) — describeEffect's
  // curated table guarantees exactly these keys are present.
  const EFFECT_MSG: Record<EffectKey, (p: Record<string, string>) => string> = {
    'sim.effect.set_url': (p) => m['sim.effect.set_url']({ url: p.url }),
    'sim.effect.set_wifi': (p) => m['sim.effect.set_wifi']({ ssid: p.ssid }),
    'sim.effect.wifi_add': (p) => m['sim.effect.wifi_add']({ ssid: p.ssid }),
    'sim.effect.nvs_set': (p) => m['sim.effect.nvs_set']({ ns: p.ns, key: p.key, val: p.val }),
    'sim.effect.net_clear': () => m['sim.effect.net_clear'](),
    'sim.effect.tx_power': (p) => m['sim.effect.tx_power']({ dbm: p.dbm }),
    'sim.effect.reboot': () => m['sim.effect.reboot'](),
    'sim.effect.refresh': () => m['sim.effect.refresh'](),
    'sim.effect.device_sleep': (p) => m['sim.effect.device_sleep']({ s: p.s }),
  }

  let scenarioId = $state(DEFAULT_C2_SCENARIO_ID)
  const result = $derived(traceC2(script, c2ScenarioById(scenarioId)))

  function effectText(fn: string, args: Arg[]): string {
    const d = describeEffect(fn, args, manifest ?? undefined)
    return d.source === 'curated' ? EFFECT_MSG[d.key](d.params) : d.text
  }
  function severing(fn: string, args: Arg[]): boolean {
    return describeEffect(fn, args, manifest ?? undefined).severing
  }
</script>

<section class="c2trace">
  <header>
    <h3>{m['sim.c2.heading']()}</h3>
    <p class="muted">{m['sim.c2.subtitle']()}</p>
  </header>

  <label class="ctl">
    <span>{m['sim.c2.scenario_label']()}</span>
    <select bind:value={scenarioId} aria-label={m['sim.c2.scenario_label']()}>
      {#each C2_SCENARIOS as sc (sc.id)}
        <option value={sc.id}>{SCENARIO_LABEL[sc.id]?.() ?? sc.id}</option>
      {/each}
    </select>
  </label>

  <p class="s8" role="note"><strong>{m['sim.c2.s8_label']()}</strong></p>

  {#if result.trace.length === 0}
    <p class="muted empty">{m['sim.c2.empty']()}</p>
  {:else}
    <ol class="trace">
      {#each result.trace as e, i (i)}
        <li class:failed={!e.ok} class:severing={severing(e.fn, e.args)}>
          <code class="call">{e.fn}({formatArgs(e.args)})</code>
          <span class="effect">{effectText(e.fn, e.args)}</span>
          <span class="ret muted">{m['sim.c2.returns_label']()} <code>{formatArg(e.ret)}</code></span>
          {#if severing(e.fn, e.args)}
            <span class="badge sev">{m['sim.c2.severing_badge']()}</span>
          {/if}
          {#if !e.ok}
            <span class="badge fail">{m['sim.c2.failed']()}</span>
          {/if}
        </li>
      {/each}
    </ol>
  {/if}

  <p class="boundary muted" role="note">{m['sim.c2.boundary']()}</p>
</section>

<style>
  .c2trace {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 0.75rem 1rem;
  }
  header h3 {
    margin: 0 0 0.2rem;
    font-size: 0.95rem;
  }
  .muted {
    color: var(--fg-muted);
    margin: 0;
  }
  .ctl {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.8rem;
    color: var(--fg-muted);
    max-width: 16rem;
  }
  .ctl select {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 6px;
    color: var(--fg);
    padding: 0.3rem 0.5rem;
    font-size: 0.88rem;
  }
  .s8 {
    border-left: 3px solid var(--warn, #d08770);
    padding: 0.35rem 0.6rem;
    font-size: 0.8rem;
    background: color-mix(in srgb, var(--warn, #d08770) 8%, transparent);
    border-radius: 0 6px 6px 0;
  }
  .trace {
    list-style: decimal;
    margin: 0;
    padding-left: 1.4rem;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .trace li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0.4rem 0.6rem;
    font-size: 0.85rem;
  }
  .trace li.failed .call {
    text-decoration: line-through;
    opacity: 0.7;
  }
  .call {
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.8rem;
    color: var(--fg);
  }
  .effect {
    flex: 1 1 16rem;
  }
  .ret {
    font-size: 0.78rem;
  }
  .ret code {
    font-family: var(--mono, ui-monospace, monospace);
  }
  .badge {
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    padding: 0.05rem 0.4rem;
    border-radius: 999px;
    white-space: nowrap;
  }
  .badge.sev {
    color: var(--warn, #d08770);
    border: 1px solid var(--warn, #d08770);
  }
  .badge.fail {
    color: var(--danger, #bf616a);
    border: 1px solid var(--danger, #bf616a);
  }
  .empty {
    font-size: 0.85rem;
  }
  .boundary {
    border-top: 1px solid var(--border);
    padding-top: 0.5rem;
    font-size: 0.78rem;
  }
</style>
