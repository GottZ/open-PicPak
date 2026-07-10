// ts-service.worker.ts — the lazy TypeScript language service for the FaaS editor, isolated in a module
// Web Worker (worker-src 'self', granted with E-A33-2; see backend/web/web.go csp). Vite emits this file
// as its OWN chunk from the `new Worker(new URL(…))` call in ts-service-client.ts, so the TypeScript
// compiler + the lib.d.ts text (the bulk of the lazy megabytes) NEVER land in the initial bundle — they
// load only after the editor idle-opts-in (mirrors the WASM idle-load of the Berry simulator, W-A33.3).
//
// It exposes, over comlink, exactly the method names the @valtown/codemirror-ts *Worker extensions call
// (updateFile / getLints / getAutocompletion / getHover), translating buffer<->wrapped positions so the
// operator sees squiggles/completions/hovers in their own coordinates.

import * as Comlink from 'comlink'
import { getAutocompletion, getHover } from '@valtown/codemirror-ts/worker'
import type { VirtualTypeScriptEnvironment } from '@typescript/vfs'
import { createFaasEnv, diagnose, OPERATOR_PATH } from './ts-service-env'
import { clampToBuffer, toEnvPos, wrapSource } from './ts-wrap'

// The real TypeScript lib.d.ts files, inlined as raw text at build time — no filesystem and no CDN at
// runtime (CSP connect-src 'self' forbids a CDN fetch). Eager glob → the text lands in THIS chunk.
const libSources = import.meta.glob('/node_modules/typescript/lib/lib.*.d.ts', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

function buildLibMap(): Map<string, string> {
  const map = new Map<string, string>()
  for (const [path, src] of Object.entries(libSources)) {
    map.set('/' + path.slice(path.lastIndexOf('/') + 1), src) // /lib.es2023.d.ts, …
  }
  return map
}

let env: VirtualTypeScriptEnvironment | null = null
let lastCode = ''

function ensureEnv(): VirtualTypeScriptEnvironment {
  if (!env) env = createFaasEnv(buildLibMap())
  return env
}

const api = {
  async initialize(): Promise<void> {
    ensureEnv()
  },

  updateFile({ code }: { path: string; code: string }): void {
    lastCode = code
    ensureEnv().updateFile(OPERATOR_PATH, wrapSource(code))
  },

  getLints({ diagnosticCodesToIgnore }: { path: string; diagnosticCodesToIgnore: number[] }) {
    return diagnose(ensureEnv(), lastCode, diagnosticCodesToIgnore)
  },

  async getAutocompletion({
    context,
  }: {
    path: string
    context: { pos: number; explicit: boolean }
  }) {
    const res = await getAutocompletion({
      env: ensureEnv(),
      path: OPERATOR_PATH,
      context: { pos: toEnvPos(context.pos), explicit: context.explicit },
    })
    if (res) {
      res.from = clampToBuffer(res.from, lastCode.length)
      if (typeof res.to === 'number') res.to = clampToBuffer(res.to, lastCode.length)
    }
    return res
  },

  getHover({ pos }: { path: string; pos: number }) {
    const h = getHover({ env: ensureEnv(), path: OPERATOR_PATH, pos: toEnvPos(pos) })
    if (!h) return h
    return { ...h, start: clampToBuffer(h.start, lastCode.length), end: clampToBuffer(h.end, lastCode.length) }
  },
}

export type FaasTsServiceApi = typeof api

Comlink.expose(api)
