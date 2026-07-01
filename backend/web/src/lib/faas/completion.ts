// CodeMirror 6 autocomplete wrapper (Design 25 §4.5). The completion SURFACE (completionOptions) lives in
// catalog.ts (CM6-free, node-testable, T7); this module only adapts it into a CM6 extension. The bound
// secret names are read through a live getter so editing the secret-bindings updates the completion
// WITHOUT re-mounting the editor — and an UNBOUND name is never offered (D25.7).

import { autocompletion, type Completion, type CompletionContext, type CompletionResult } from '@codemirror/autocomplete'
import { completionOptions } from './catalog'

/** A CM6 autocompletion extension fed by the curated FaaS scope + the function's currently-bound secrets. */
export function faasAutocomplete(getBound: () => readonly string[]) {
  return autocompletion({
    override: [
      (ctx: CompletionContext): CompletionResult | null => {
        // match dotted paths (cap.secrets.foo) so the whole path completes, not just the trailing word.
        const word = ctx.matchBefore(/[\w.$]+/)
        if (!word || (word.from === word.to && !ctx.explicit)) return null
        const options: Completion[] = completionOptions(getBound()).map((o) => ({
          label: o.label,
          detail: o.detail,
          info: o.info,
          type: o.label.startsWith('cap.secrets.') ? 'variable' : 'function',
        }))
        return { from: word.from, options, validFor: /^[\w.$]*$/ }
      },
    ],
  })
}
