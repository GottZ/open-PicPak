// CodeMirror 6 autocomplete wrapper (Design 23 §4.3). The completion SURFACE (completionOptions) lives
// in catalog.ts (CM6-free, node-testable, T1); this module only adapts it into a CM6 extension. The
// completion list is the manifest only — the operator cannot tab-complete a name the executor lacks.

import {
  autocompletion,
  snippetCompletion,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from '@codemirror/autocomplete'
import { type Manifest, completionOptions } from './catalog'

/** A CodeMirror 6 autocompletion extension fed by the manifest (no other source — the safe subset only). */
export function berryAutocomplete(manifest: Manifest) {
  const options: Completion[] = completionOptions(manifest).map((o) => {
    const base: Completion = {
      label: o.label,
      detail: o.severing ? `${o.detail}  ⚠ severing` : o.detail,
      info: o.info,
      type: o.class === 'builtin' ? 'keyword' : o.severing ? 'keyword' : 'function',
    }
    // capabilities complete as snippets with per-param tab-stops (A33 §4.1.3); builtins/keywords stay
    // plain (no call to expand). apply/label live on the snippet completion; the surface is unchanged.
    return o.class === 'builtin' ? base : snippetCompletion(o.snippet, base)
  })
  return autocompletion({
    override: [
      (ctx: CompletionContext): CompletionResult | null => {
        const word = ctx.matchBefore(/[A-Za-z_]\w*/)
        if (!word || (word.from === word.to && !ctx.explicit)) return null
        return { from: word.from, options, validFor: /^[A-Za-z_]\w*$/ }
      },
    ],
  })
}
