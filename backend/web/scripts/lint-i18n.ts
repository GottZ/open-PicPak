#!/usr/bin/env bun
/**
 * A34.5 — Gate "kein hartkodierter UI-String".
 *
 * Scannt jede src-Svelte-Datei auf user-sichtbare, hartkodierte Strings, die
 * an Paraglide (messages/*.json, Zugriff via `m['key']()`) vorbeigehen:
 *
 *   (a) Text-Nodes im Markup mit Wortcharakter (echte Buchstabenläufe, nicht
 *       nur `{...}`-Interpolationen, Whitespace oder Symbole).
 *   (b) Statische `title=` / `placeholder=` / `aria-label=`-Attribute mit
 *       Literalwert (also NICHT `title={m[...]()}`).
 *
 * Läuft mit Bordmitteln (Svelte-Compiler-Parser, kein neuer npm-Dep). In
 * `package.json` an `check` gehängt → der Docker-Build (`bun run check`) erzwingt
 * das Gate. Exit 1 bei Fund, Ausgabe je Fund `datei:zeile:spalte  Literal`.
 *
 * ── Whitelist (dokumentierte A34.2-Ausnahmen) ──────────────────────────────
 * Ein Text/Attribut-Wert ist NUR dann ein Fund, wenn er ein Token mit einem
 * Buchstabenlauf ≥2 enthält, das nicht gewhitelistet ist. Damit fallen die
 * folgenden legitimen Technik-Tokens automatisch oder explizit raus:
 *   - Kürzel/Glyphen ohne 2-Buchstaben-Lauf: `n/a`, `—`, `✕`, `▸`, Icon-Glyphen
 *     → automatisch (kein Lauf ≥2 Buchstaben).
 *   - Cron-Beispiele (`* / 5 * * * *`), reine Zahlen/Symbole → automatisch.
 *   - `v`-Prefix-Versionen (`v1.2.3`, `1.0`) → automatisch (nur 1-Buchstaben-Lauf)
 *     bzw. via VERSION-Pattern.
 *   - Serials / Hex-Muster (`deadbeef`, MAC `aa:bb:cc`) → HEX/MAC-Pattern.
 *   - Brand `open-picpak`, Fachwörter `severing`, `render-phase` → WORD-Liste.
 * Zusätzlich: `<!-- i18n-ignore -->` als voranstehender Geschwister-Kommentar
 * unterdrückt alle Funde im unmittelbar folgenden Knoten (inkl. Attribute und
 * Kindern) — für den seltenen Fall eines legitimen, nicht token-whitelistbaren
 * Literals. Marker dokumentiert, bewusst grob-granular (ganzes Element).
 */
import { parse } from 'svelte/compiler'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, dirname, relative } from 'node:path'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
const WEB_ROOT = join(SCRIPT_DIR, '..')
const SRC_DIR = join(WEB_ROOT, 'src')

// ── Whitelist ──────────────────────────────────────────────────────────────
// Exakte Tokens (case-insensitive), die trotz Buchstabenlauf legitim sind.
const WORD_WHITELIST = new Set<string>([
  'open-picpak', // Brand
  'severing', //   Fachbegriff (Verbindungs-Abbruch-Zustand)
  'render-phase', // Fachbegriff (Svelte-Render-Phase)
])
// Muster für generierte/technische Tokens (case-insensitive geprüft).
const PATTERN_WHITELIST: RegExp[] = [
  /^v?\d[\d.]*[a-z]?$/i, //          Versionen: v1.2.3, 1.0, 2b
  /^[0-9a-f]{6,}$/i, //              Hex / Serials: deadbeef
  /^[0-9a-f]{2}([:-][0-9a-f]{2})+$/i, // MAC/Serial: aa:bb:cc / 12-34-56
]
// Ein Token gilt als "Wort" (kandidat) nur mit Buchstabenlauf ≥2 (Latin +
// Latin-1-Supplement/Extended-A für Umlaute/Akzente).
const LETTER_RUN = /[A-Za-zÀ-ɏ]{2,}/
const ATTRS = new Set(['title', 'placeholder', 'aria-label'])
// Exakte Attribut-Literale, die Format-/Beispielwerte sind (keine Prosa): sie
// bleiben — konsistent mit A34.2, das numerische Beispiel-Placeholder wie
// `1800` ebenfalls literal ließ, während echte Hinweis-Placeholder Keys bekamen
// (z. B. `onboard.ph_optional`). Trimmed-Vergleich gegen den Attributwert.
const TECH_PLACEHOLDERS = new Set<string>([
  'm h dom mon dow', // Cron-Feld-Legende (min h DoM Mon DoW) — spec-Ausnahme "cron-Beispiele"
  'host.example:8123', // Host:Port-Format-Beispiel
  'RFC3339', //          Zeitstempel-Format-Name (Standard-Bezeichner)
  'PP-001', //           Serial-Format-Beispiel (Brand-Prefix)
  'https://…', //        URL-Format-Hinweis
  'stable', //           Beispiel-Kanalwert (Release-Channel-Bezeichner)
])
// Bekannte, noch nicht migrierte Rest-Literale in Dateien, die eine PARALLELE
// Welle besitzt (hier nicht anfassbar) — sie werden auf jener Welle migriert.
// Bewusst datei+literal-genau statt Verzeichnis-Skip, damit NEUE Literale in
// denselben Dateien weiterhin gefangen werden. Entfernen, sobald die
// Partnerwelle gemerged ist (dann sind die Literale weg → Einträge tot).
// Leer seit dem A34.5-Merge-Nachzug (beide TemplatePicker-aria-labels sind migriert);
// der Mechanismus bleibt für künftige Wellen-Grenzen bestehen.
const DEFERRED: ReadonlyArray<{ file: string; literal: string; owner: string }> = []

type Finding = { file: string; line: number; col: number; literal: string; kind: string }

function stripEdges(tok: string): string {
  return tok.replace(/^[^\p{L}\p{N}]+|[^\p{L}\p{N}]+$/gu, '')
}

/** Liefert die nicht-whitelisteten "Wort"-Tokens eines Strings (leer = sauber). */
function offenders(text: string): string[] {
  const out: string[] = []
  for (const raw of text.split(/\s+/)) {
    const tok = stripEdges(raw)
    if (!tok || !LETTER_RUN.test(tok)) continue
    if (WORD_WHITELIST.has(tok.toLowerCase())) continue
    if (PATTERN_WHITELIST.some((p) => p.test(tok))) continue
    out.push(tok)
  }
  return out
}

/** offset → 1-basiert {line, col} über einen Zeilenanfangs-Index. */
function locator(source: string) {
  const starts = [0]
  for (let i = 0; i < source.length; i++) if (source[i] === '\n') starts.push(i + 1)
  return (offset: number) => {
    let lo = 0
    let hi = starts.length - 1
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1
      if (starts[mid] <= offset) lo = mid
      else hi = mid - 1
    }
    return { line: lo + 1, col: offset - starts[lo] + 1 }
  }
}

/** Alle Sibling-Listen (Fragmente/Block-Zweige) eines AST-Knotens. */
function siblingLists(node: any): any[][] {
  const out: any[][] = []
  const push = (f: any) => {
    if (f && Array.isArray(f.nodes)) out.push(f.nodes)
  }
  if (node.type === 'Fragment' && Array.isArray(node.nodes)) out.push(node.nodes)
  push(node.fragment) //                 Element/Component/KeyBlock-Kinder
  push(node.consequent) //               IfBlock then
  push(node.alternate) //                IfBlock else
  push(node.body) //                     EachBlock/SnippetBlock
  push(node.fallback) //                 EachBlock {:else}
  push(node.pending) //                  AwaitBlock
  push(node.then)
  push(node.catch)
  return out
}

function lintFile(absPath: string, rel: string, findings: Finding[]) {
  const source = readFileSync(absPath, 'utf8')
  const at = locator(source)
  let ast: any
  try {
    ast = parse(source, { modern: true })
  } catch (err) {
    findings.push({
      file: rel,
      line: 1,
      col: 1,
      literal: `<parse error: ${(err as Error).message}>`,
      kind: 'parse',
    })
    return
  }

  const reportText = (node: any) => {
    const bad = offenders(node.data)
    if (bad.length) {
      const { line, col } = at(node.start)
      findings.push({ file: rel, line, col, literal: node.data.trim(), kind: 'markup' })
    }
  }
  const reportAttr = (attr: any) => {
    if (!ATTRS.has(attr.name)) return
    if (!Array.isArray(attr.value)) return //         {expr} oder boolean → kein Literal
    const literal = attr.value
      .filter((v: any) => v.type === 'Text')
      .map((v: any) => v.data)
      .join('')
    if (TECH_PLACEHOLDERS.has(literal.trim())) return
    if (offenders(literal).length) {
      const { line, col } = at(attr.start)
      findings.push({ file: rel, line, col, literal: `${attr.name}="${literal.trim()}"`, kind: 'attr' })
    }
  }

  const visitNode = (node: any, ignored: boolean) => {
    if (!node || typeof node.type !== 'string') return
    if (!ignored) {
      if (node.type === 'Text') reportText(node)
      if (Array.isArray(node.attributes)) for (const a of node.attributes) reportAttr(a)
    }
    for (const list of siblingLists(node)) visitList(list, ignored)
  }
  const visitList = (list: any[], ignored: boolean) => {
    let ignoreNext = false
    for (const item of list) {
      if (item?.type === 'Comment' && /i18n-ignore/i.test(item.data)) {
        ignoreNext = true
        continue
      }
      // Whitespace-Text zwischen Kommentar und Ziel verbraucht die Ignore nicht.
      if (item?.type === 'Text' && item.data.trim() === '') {
        visitNode(item, ignored)
        continue
      }
      visitNode(item, ignored || ignoreNext)
      ignoreNext = false
    }
  }

  visitNode(ast.fragment, false)
}

function svelteFiles(dir: string, acc: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) svelteFiles(full, acc)
    else if (entry.endsWith('.svelte')) acc.push(full)
  }
  return acc
}

const raw: Finding[] = []
for (const abs of svelteFiles(SRC_DIR).sort()) {
  lintFile(abs, relative(WEB_ROOT, abs).split(/[\\/]/).join('/'), raw)
}
// Bekannte Partnerwellen-Deferrals herausfiltern (datei+literal-genau).
const findings = raw.filter(
  (f) => !DEFERRED.some((d) => d.file === f.file && d.literal === f.literal),
)

if (findings.length) {
  console.error(`i18n-lint: ${findings.length} hartkodierte(r) UI-String(s) gefunden:\n`)
  for (const f of findings) {
    console.error(`  ${f.file}:${f.line}:${f.col}  [${f.kind}]  ${JSON.stringify(f.literal)}`)
  }
  console.error(
    `\nMigrieren → Message-Key (messages/*.json, Zugriff m['<area>.<komponente>.<slug>']()),` +
      ` legitimes Technik-Token → Whitelist in scripts/lint-i18n.ts oder <!-- i18n-ignore -->.`,
  )
  process.exit(1)
}
console.log('i18n-lint: keine hartkodierten UI-Strings.')
