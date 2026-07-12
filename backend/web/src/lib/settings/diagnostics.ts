// Diagnostics-Panel Anzeige-Logik (design 02-settings-spa §7-W3, Umsetzungsnote
// E5=c: NUR GET /api/config, /api/onboard/defaults bewusst weggelassen —
// dupliziert die /onboard-Seite). Pure, kein DOM — die "leer -> nicht
// gesetzt"-Klassifikation ist das W3-Gate: eine unset env-URL muss als
// settings.diag.not_set-Text rendern, NIE als leerer String (eine leere
// Zeichenkette säße sonst optisch neben einem echten Wert und läse sich wie ein
// erfolgreich geladenes "nichts", statt als "hier fehlt Config"). Extrahiert,
// damit die Klassifikation ohne Component-Render-Harness testbar ist
// (Muster lib/settings/secrets.ts, N2 pinned in diagnostics.test.ts).

import type { ConfigResponse } from '../api/types'

/** One row of the /api/config diagnostic display. notSet marks an unset env (empty string). */
export interface DiagRow {
  labelKey: 'settings.diag.field.grafana_base_url' | 'settings.diag.field.webhook_base_url'
  value: string
  notSet: boolean
}

/** GET /api/config (auth-only, telemetry_http.go:66/144-151) -> the ordered display rows. */
export function diagRows(cfg: ConfigResponse): DiagRow[] {
  return [
    {
      labelKey: 'settings.diag.field.grafana_base_url',
      value: cfg.grafana_base_url,
      notSet: cfg.grafana_base_url === '',
    },
    {
      labelKey: 'settings.diag.field.webhook_base_url',
      value: cfg.webhook_base_url,
      notSet: cfg.webhook_base_url === '',
    },
  ]
}
