<script lang="ts">
  // Diagnostics-Panel (design 02-settings-spa §7-W3, Umsetzungsnote E5=c) —
  // read-only "warum ist Feature X dark?"-Diagnose über GET /api/config. NUR
  // config (grafana/webhook base URL); GET /api/onboard/defaults ist bewusst
  // WEGGELASSEN — dupliziert die /onboard-Seite (E5-Umsetzungsnote).
  //
  // PLATZIERUNG (SettingsHome): GET /api/config ist AUTH-ONLY
  // (adminhttp.Auth(pool), telemetry_http.go:66), NICHT RequireAdmin — jeder
  // eingeloggte Operator darf lesen (mirrors die Telemetry-Dashboard-Routen,
  // deren Kommentar telemetry_http.go:15-17 dasselbe Q5-Muster benennt: "a
  // read-only operator must reach the dashboard"). Deshalb hängt dieses Panel
  // in SettingsHome AUSSERHALB des is_admin-Zweigs — auch ein Nicht-Admin sieht
  // die Diagnose, nur SecretsPanel bleibt admin-gated. Die B5-Falle (leere
  // Tabelle liest sich wie eine stille Lüge bei einem 403) greift hier nicht:
  // ein Nicht-Admin bekommt von dieser Route nie ein 403.
  //
  // W3-GATE: eine leere env-Zeichenkette rendert als settings.diag.not_set-Text
  // (gedimmt), NIE als leerer String — diagRows() (lib/settings/diagnostics.ts)
  // trägt diese Klassifikation als pure, getestete Funktion (N2).
  import { onMount } from 'svelte'
  import StateView from '../../lib/StateView.svelte'
  import { Resource } from '../../lib/resource.svelte'
  import { apiFetch } from '../../lib/api'
  import { diagRows } from '../../lib/settings/diagnostics'
  import type { ConfigResponse } from '../../lib/api/types'
  import { m } from '../../paraglide/messages.js'

  // env ändert sich nur mit einem Prozess-Neustart — kein Reload-Zwang, ein
  // einmaliger onMount-Load reicht (Muster SecretsPanel.svelte:104).
  const config = new Resource<ConfigResponse>(() => apiFetch<ConfigResponse>('/api/config'))

  onMount(() => void config.load())
</script>

<section class="diagnostics" aria-label={m['settings.diag.heading']()}>
  <h2>{m['settings.diag.heading']()}</h2>

  <StateView resource={config}>
    {#snippet ready(data)}
      <dl>
        {#each diagRows(data) as row (row.labelKey)}
          <div class="row">
            <dt>{m[row.labelKey]()}</dt>
            <dd class:muted={row.notSet}>{row.notSet ? m['settings.diag.not_set']() : row.value}</dd>
          </div>
        {/each}
      </dl>
    {/snippet}
  </StateView>
</section>

<style>
  .diagnostics {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 600;
  }
  dl {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    margin: 0;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 0.75rem;
    max-width: 32rem;
  }
  .row {
    display: flex;
    justify-content: space-between;
    gap: 1rem;
    font-size: 0.875rem;
  }
  dt {
    color: var(--fg-muted);
  }
  dd {
    margin: 0;
    text-align: right;
    word-break: break-all;
  }
  dd.muted {
    color: var(--fg-muted);
    font-style: italic;
  }
</style>
