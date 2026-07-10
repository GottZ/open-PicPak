// ts-service-client.ts — main-thread wiring for the FaaS TypeScript language service. This module is
// reached ONLY through a dynamic import() (editor.ts#enableTypeService), so comlink and the
// @valtown/codemirror-ts client extensions stay out of the initial bundle; and `new Worker(new URL(…))`
// makes Vite split ts-service.worker.ts (TypeScript compiler + libs) into its own lazy chunk.

import * as Comlink from 'comlink'
import {
  tsFacetWorker,
  tsSyncWorker,
  tsLinterWorker,
  tsAutocompleteWorker,
  tsHoverWorker,
} from '@valtown/codemirror-ts'
import type { WorkerShape } from '@valtown/codemirror-ts/worker'
import type { Extension } from '@codemirror/state'
import type { CompletionSource } from '@codemirror/autocomplete'
import { OPERATOR_PATH } from './ts-wrap'

export interface TsService {
  /** Facet + sync + linter + hover — drop straight into the language-service compartment. */
  extensions: Extension[]
  /** Type-aware completion source; the editor combines it with the static catalog source. */
  completionSource: CompletionSource
  /** Tear down the worker. */
  dispose(): void
}

/**
 * Spin up the worker, wait for its TypeScript environment to initialise, and return the wired pieces.
 * Rejects if the worker or its init fails — the caller keeps the editor fail-open.
 */
export async function createTsService(): Promise<TsService> {
  const worker = new Worker(new URL('./ts-service.worker.ts', import.meta.url), { type: 'module' })
  const remote = Comlink.wrap(worker) as unknown as WorkerShape
  await remote.initialize()
  const extensions: Extension[] = [
    tsFacetWorker.of({ path: OPERATOR_PATH, worker: remote }),
    tsSyncWorker(),
    tsLinterWorker(),
    tsHoverWorker(),
  ]
  return {
    extensions,
    completionSource: tsAutocompleteWorker(),
    dispose: () => worker.terminate(),
  }
}
