<script lang="ts">
  import { apiFetch } from '../../lib/api'
  import { Resource } from '../../lib/resource.svelte'
  import { activeLocale } from '../../lib/i18n'
  import {
    type DeviceTelemetryResponse,
    type AppConfig,
    type HistoryPoint,
    healthGlyph,
    healthClass,
    noDataLabel,
    metricLabel,
    battLabel,
    channelDisplay,
    ageLabel,
    sparklinePath,
    grafanaLink,
    loadAppConfig,
  } from './fleet'
  import { m } from '../../paraglide/messages.js'

  // Per-device card (Design 22 §4.4): the latest full telemetry row (sentinel-aware n/a), a few fixed-
  // window sparklines, a recent-history table, the brownout/bad-boots reason chips, and the
  // "deep dive → Grafana" link (hidden when grafana_base_url is unset — no URL is ever hardcoded, D22.13/
  // air-gap). Every device-sourced string (label/reset_reason/version) renders as a TEXT NODE: {@html} is
  // banned (D19.10). M9: this is latest-values + a 72 h glance, NOT a Grafana replacement.

  let { serial, onclose }: { serial: string; onclose?: () => void } = $props()

  const card = new Resource<DeviceTelemetryResponse>(() =>
    apiFetch<DeviceTelemetryResponse>(`/api/devices/${encodeURIComponent(serial)}/telemetry`),
  )

  let grafanaBase = $state('')
  void loadAppConfig(() => apiFetch<AppConfig>('/api/config')).then((c) => (grafanaBase = c.grafana_base_url))

  // (re)load whenever the selected serial changes
  let lastSerial = ''
  $effect(() => {
    if (serial !== lastSerial) {
      lastSerial = serial
      void card.load()
    }
  })

  const nowMs = Date.now()

  // sparklines: history is newest-first → reverse to oldest→newest for left-to-right plotting.
  function series(hist: HistoryPoint[], pick: (p: HistoryPoint) => number | null): (number | null)[] {
    return [...hist].reverse().map(pick)
  }
  const SPARK_W = 130
  const SPARK_H = 28

  function tsLabel(iso: string): string {
    const d = new Date(iso)
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(activeLocale())
  }
</script>

<aside class="card">
  <header>
    <h2 class="mono">{serial}</h2>
    {#if onclose}<button class="close" onclick={onclose} aria-label={m['fleet.card.close']()}>✕</button>{/if}
  </header>

  {#if card.status === 'loading' || card.status === 'idle'}
    <p class="muted" aria-busy="true">{m['fleet.card.loading']()}</p>
  {:else if card.status === 'error'}
    <div class="error" role="alert">
      <p>{card.error?.message}</p>
      {#if card.error?.requestId}<p class="muted">{m['app.request']({ id: card.error.requestId })}</p>{/if}
    </div>
  {:else if card.data}
    {@const d = card.data}
    {@const ch = channelDisplay(d.fleet)}
    {@const link = grafanaLink(grafanaBase, serial)}

    <div class="head">
      <span class="glyph {healthClass(d.fleet.health)}" title={d.fleet.health}>{healthGlyph(d.fleet.health)}</span>
      <span class="hlabel {healthClass(d.fleet.health)}">{d.fleet.health}</span>
      {#if d.fleet.label}<span class="muted">· {d.fleet.label}</span>{/if}
      <span class="muted">· {ch.text}{#if ch.mismatch}<span class="mismatch"> ≠</span>{/if}</span>
    </div>

    <!-- reason chips: NO_DATA split, verdict reasons, the cross-row brownout-rollback corroboration -->
    <div class="chips">
      {#if !d.fleet.has_data}
        <span class="chip muted">{noDataLabel(d.fleet)}</span>
      {/if}
      {#each d.fleet.reasons as r (r)}
        {#if d.fleet.has_data}<span class="chip {healthClass(d.fleet.health)}">{r}</span>{/if}
      {/each}
      {#if d.rollback_brownout}<span class="chip danger">{m['fleet.card.rollback_brownout']()}</span>{/if}
    </div>

    {#if d.latest}
      {@const L = d.latest}
      <dl class="grid">
        <div><dt>{m['fleet.card.dt.version']()}</dt><dd class="mono">{L.running_ver || 'n/a'}</dd></div>
        <div><dt>{m['fleet.card.dt.battery']()}</dt><dd>{battLabel(L.batt_pct, L.batt_mv)}</dd></div>
        <div><dt>{m['fleet.card.dt.uptime']()}</dt><dd>{metricLabel(L.uptime_ms)}</dd></div>
        <div><dt>{m['fleet.card.dt.boot_count']()}</dt><dd>{metricLabel(L.boot_count)}</dd></div>
        <div><dt>{m['fleet.card.dt.bad_boots']()}</dt><dd>{metricLabel(L.bad_boots)}</dd></div>
        <div><dt>{m['fleet.card.dt.reset_reason']()}</dt><dd>{L.reset_reason ?? 'n/a'}</dd></div>
        <div><dt>{m['fleet.card.dt.usb']()}</dt><dd>{L.usb === null ? 'n/a' : L.usb ? m['fleet.card.yes']() : m['fleet.card.no']()}</dd></div>
        <div><dt>{m['fleet.card.dt.diag_ota_rr']()}</dt><dd>{metricLabel(L.diag_ota_rr)}</dd></div>
        <div><dt>{m['fleet.card.dt.diag_runstate']()}</dt><dd>{metricLabel(L.diag_runstate)}</dd></div>
        <div><dt>{m['fleet.card.dt.rssi']()}</dt><dd>{metricLabel(L.extra.rssi, ' dBm')}</dd></div>
        <div><dt>{m['fleet.card.dt.heap']()}</dt><dd>{metricLabel(L.extra.heap)}</dd></div>
        <div><dt>{m['fleet.card.dt.temp']()}</dt><dd>{metricLabel(L.extra.temp, '°C')}</dd></div>
      </dl>

      <div class="sparks">
        {#each [{ k: 'batt', label: m['fleet.card.spark.batt'](), pick: (p: HistoryPoint) => p.batt_pct }, { k: 'uptime', label: m['fleet.card.spark.uptime'](), pick: (p: HistoryPoint) => p.uptime_ms }, { k: 'bad_boots', label: m['fleet.card.spark.bad_boots'](), pick: (p: HistoryPoint) => p.bad_boots }] as s (s.k)}
          {@const path = sparklinePath(series(d.history, s.pick), SPARK_W, SPARK_H)}
          <div class="spark">
            <span class="muted sk">{s.label}</span>
            {#if path}
              <svg viewBox="0 0 {SPARK_W} {SPARK_H}" width={SPARK_W} height={SPARK_H} role="img" aria-label={m['fleet.card.spark_trend']({ label: s.label })}>
                <path d={path} fill="none" stroke="var(--accent)" stroke-width="1.5" />
              </svg>
            {:else}
              <span class="muted">n/a</span>
            {/if}
          </div>
        {/each}
      </div>

      {#if d.history.length > 0}
        <table class="hist">
          <thead><tr><th>{m['fleet.card.hist.time']()}</th><th>{m['fleet.card.hist.batt']()}</th><th>{m['fleet.card.hist.uptime']()}</th><th>{m['fleet.card.hist.bad_boots']()}</th><th>{m['fleet.card.hist.reset']()}</th></tr></thead>
          <tbody>
            {#each d.history as p (p.time)}
              <tr>
                <td class="muted">{tsLabel(p.time)}</td>
                <td>{battLabel(p.batt_pct, p.batt_mv)}</td>
                <td>{metricLabel(p.uptime_ms)}</td>
                <td>{metricLabel(p.bad_boots)}</td>
                <td>{p.reset_reason ?? 'n/a'}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    {:else}
      <p class="muted">{m['fleet.card.no_telemetry']({ reason: noDataLabel(d.fleet) })}</p>
    {/if}

    <footer class="foot muted">
      <span>{m['fleet.card.report']({ age: ageLabel(d.fleet.reg_last_seen, nowMs) })}</span>
      <span>{m['fleet.card.c2']({ age: ageLabel(d.fleet.c2_last_seen, nowMs) })}</span>
      {#if link}<a class="grafana" href={link} target="_blank" rel="noopener noreferrer">{m['fleet.card.grafana']()}</a>{/if}
    </footer>
  {/if}
</aside>

<style>
  .card {
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
    background: var(--bg);
  }
  header {
    display: flex;
    align-items: center;
    border-bottom: 1px solid var(--border);
    padding-bottom: 0.4rem;
  }
  h2 {
    margin: 0;
    font-size: 1.05rem;
    flex: 1;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 0.4rem;
    font-size: 0.9rem;
  }
  .glyph {
    font-size: 1.1rem;
  }
  .hlabel {
    font-size: 0.75rem;
    letter-spacing: 0.04em;
  }
  .chips {
    display: flex;
    gap: 0.4rem;
    flex-wrap: wrap;
  }
  .chip {
    font-size: 0.72rem;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--border);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(11rem, 1fr));
    gap: 0.3rem 1rem;
    margin: 0;
    font-size: 0.85rem;
  }
  .grid div {
    display: flex;
    justify-content: space-between;
    gap: 0.5rem;
    border-bottom: 1px solid var(--border);
    padding: 0.15rem 0;
  }
  dt {
    color: var(--fg-muted);
  }
  dd {
    margin: 0;
    font-family: monospace;
  }
  .sparks {
    display: flex;
    gap: 1.25rem;
    flex-wrap: wrap;
  }
  .spark {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
  }
  .sk {
    font-size: 0.7rem;
  }
  .hist {
    border-collapse: collapse;
    width: 100%;
    font-size: 0.8rem;
  }
  .hist th,
  .hist td {
    text-align: left;
    padding: 0.25rem 0.5rem;
    border-bottom: 1px solid var(--border);
  }
  .hist th {
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--fg-muted);
  }
  .foot {
    display: flex;
    gap: 1.25rem;
    font-size: 0.76rem;
    align-items: center;
    flex-wrap: wrap;
  }
  .grafana {
    color: var(--accent);
    margin-left: auto;
  }
  .ok {
    color: var(--ok);
  }
  .warn {
    color: var(--warn);
  }
  .danger {
    color: var(--danger);
  }
  .muted {
    color: var(--fg-muted);
  }
  .mono {
    font-family: monospace;
  }
  .mismatch {
    color: var(--warn);
  }
  .error {
    border: 1px solid var(--danger);
    border-radius: 6px;
    padding: 0.5rem 0.75rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .close {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg);
    border-radius: 6px;
    padding: 0.1rem 0.5rem;
    cursor: pointer;
  }
  .close:hover {
    border-color: var(--accent);
  }
</style>
