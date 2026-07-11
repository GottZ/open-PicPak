#!/usr/bin/env bun
/**
 * A33.5b — Gate "kein {@html}" (design/33 §5 S7).
 *
 * descriptions, Effekt-Texte und Trace-Strings sind operator-/vorlagen-beeinflusst
 * (mehrsprachige Template-Beschreibungen, Berry-Quelltext). Sie dürfen NIE über
 * Sveltes `{@html …}` gerendert werden — nur als Text-Nodes (Svelte escaped `{…}`
 * automatisch). Dieses Gate parst jede src-Svelte-Datei mit dem Svelte-Compiler und
 * bricht ab, sobald ein `HtmlTag`-Knoten ({@html …}) im AST vorkommt. An `check`
 * gehängt → der Docker-Build erzwingt S7.
 *
 * AST-basiert (nicht textuell), damit die Erwähnung „{@html}" in Kommentaren/Doku
 * kein Fehlalarm ist — nur echte Verwendung im Markup zählt. Bordmittel (kein neuer
 * Dep, Parser wie in lint-i18n.ts). Exit 1 bei Fund, Ausgabe je Fund `datei:zeile:spalte`.
 * Ein legitimer Bedarf existiert in dieser Codebasis nicht — es gibt keine Whitelist.
 */
import { parse } from 'svelte/compiler'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, dirname, relative } from 'node:path'

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url))
const WEB_ROOT = join(SCRIPT_DIR, '..')
const SRC_DIR = join(WEB_ROOT, 'src')

type Finding = { file: string; line: number; col: number }

/** offset → 1-basiert {line, col} (identisch zu lint-i18n.ts). */
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

/** Tief durch den AST laufen und jeden {@html …}-Knoten (type 'HtmlTag') sammeln. */
function walk(node: unknown, onHtml: (start: number) => void): void {
  if (!node || typeof node !== 'object') return
  const n = node as Record<string, unknown>
  if (n.type === 'HtmlTag' && typeof n.start === 'number') onHtml(n.start)
  for (const key of Object.keys(n)) {
    const v = n[key]
    if (Array.isArray(v)) for (const item of v) walk(item, onHtml)
    else if (v && typeof v === 'object') walk(v, onHtml)
  }
}

function svelteFiles(dir: string, acc: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) svelteFiles(full, acc)
    else if (entry.endsWith('.svelte')) acc.push(full)
  }
  return acc
}

const findings: Finding[] = []
for (const abs of svelteFiles(SRC_DIR).sort()) {
  const rel = relative(WEB_ROOT, abs).split(/[\\/]/).join('/')
  const source = readFileSync(abs, 'utf8')
  const at = locator(source)
  const ast = parse(source, { modern: true })
  walk(ast.fragment, (start) => {
    const { line, col } = at(start)
    findings.push({ file: rel, line, col })
  })
}

if (findings.length) {
  console.error(`no-html-lint: ${findings.length} verbotene(s) {@html}-Tag(s) gefunden (S7):\n`)
  for (const f of findings) console.error(`  ${f.file}:${f.line}:${f.col}`)
  console.error(`\nNur Text-Nodes verwenden ({…}); operator-/vorlagen-beeinflusste Strings nie als HTML rendern.`)
  process.exit(1)
}
console.log('no-html-lint: kein {@html}.')
