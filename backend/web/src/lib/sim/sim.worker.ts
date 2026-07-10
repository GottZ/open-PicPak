// Berry-WASM simulator Worker (design/33 §4.2/§4.3, E-A33-2). Loads the pinned
// VM module (backend/web/public/picpak-berry.mjs, a build product — sim/build.sh)
// and drives the sim_* wrapper API. Runs off the UI thread; the in-WASM 100 ms
// wall-clock deadline bounds any single run, so this thread never hangs.
import type { SimRequest, SimResponse } from './protocol'

// Worker global, typed locally: pulling the "webworker" lib clashes with the DOM
// lib svelte-check runs under, so we cast instead of switching libs.
const ctx = globalThis as unknown as {
  onmessage: ((e: MessageEvent<SimRequest>) => void) | null
  postMessage: (msg: SimResponse, transfer?: Transferable[]) => void
}

// Minimal shape of the Emscripten module we use (MODULARIZE + EXPORT_ES6).
interface BerryModule {
  ccall: (
    name: string,
    ret: string | null,
    argTypes: string[],
    args: unknown[],
  ) => number | string
  HEAPU8: Uint8Array
}

const FB_BYTES = 30000
// Public asset, not a bundled module: resolved at runtime, so @vite-ignore keeps
// Vite from trying to pre-bundle it and TS from resolving the path statically.
const MODULE_URL = '/picpak-berry.mjs'

let modPromise: Promise<BerryModule> | null = null

async function getModule(): Promise<BerryModule> {
  if (!modPromise) {
    modPromise = (async () => {
      const url = MODULE_URL
      const factory = (await import(/* @vite-ignore */ url)).default
      return (await factory()) as BerryModule
    })()
  }
  return modPromise
}

function reset(mod: BerryModule) {
  mod.ccall('sim_reset', null, ['number'], [0])
}
function setDev(mod: BerryModule, json: string) {
  mod.ccall('sim_set_dev', null, ['string'], [json])
}
function errText(mod: BerryModule): string {
  return mod.ccall('sim_error', 'string', [], []) as string
}
function readFb(mod: BerryModule): Uint8Array {
  const ptr = mod.ccall('sim_fb', 'number', [], []) as number
  // Copy out of the WASM heap (the view is invalidated by memory growth / next run).
  return mod.HEAPU8.slice(ptr, ptr + FB_BYTES)
}

ctx.onmessage = async (e: MessageEvent<SimRequest>) => {
  const req = e.data
  try {
    const mod = await getModule()
    reset(mod)
    if (req.kind === 'compileOnly') {
      const rc = mod.ccall('sim_compile_only', 'number', ['string'], [req.source]) as number
      ctx.postMessage({ id: req.id, rc, error: rc === 0 ? '' : errText(mod) })
      return
    }
    if (req.dev) setDev(mod, req.dev)
    const rc = mod.ccall('sim_run', 'number', ['string'], [req.source]) as number
    if (rc === 0) {
      const fb = readFb(mod)
      ctx.postMessage({ id: req.id, rc, error: '', fb }, [fb.buffer])
    } else {
      ctx.postMessage({ id: req.id, rc, error: errText(mod) })
    }
  } catch (err) {
    ctx.postMessage({ id: req.id, rc: -1, error: `sim worker: ${String(err)}` })
  }
}
