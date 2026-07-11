// S7 / probe (d) — template descriptions, effect summaries and Berry source are template-authored and
// operator-influenced. They must reach the DOM only as Svelte text-node interpolations ({…}, auto-escaped),
// never through {@html}. The vitest env here is node (no DOM to assert runtime escaping), so this is the
// structural gate: the gallery components carry no {@html}, and the description flows through a text node.
// The whole-tree enforcement is scripts/lint-no-html.ts wired into `bun run check`; this pins the two new
// surfaces in the vitest suite too. Red: a `{@html description}` in either file fails the first assertion.

import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

const ROOT = process.cwd() // backend/web
const card = readFileSync(join(ROOT, 'src/lib/gallery/GalleryCard.svelte'), 'utf8')
const home = readFileSync(join(ROOT, 'src/routes/gallery/GalleryHome.svelte'), 'utf8')

describe('gallery components never use {@html} (S7, probe d)', () => {
  it('GalleryCard.svelte has no {@html}', () => {
    expect(card.includes('{@html')).toBe(false)
  })

  it('GalleryHome.svelte has no {@html}', () => {
    expect(home.includes('{@html')).toBe(false)
  })

  it('the card renders the (template-authored) description as a text node', () => {
    // The description is bound to a text interpolation — a <script> in it renders as literal text.
    expect(card).toMatch(/\{description\}/)
  })
})
