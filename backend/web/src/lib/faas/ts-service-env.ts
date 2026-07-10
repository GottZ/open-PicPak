// ts-service-env.ts — builds the @typescript/vfs environment the FaaS editor type-checks against, plus
// the shared `diagnose` path. Imports the TypeScript compiler + vfs, so it is only ever reached through
// the lazy worker chunk (ts-service.worker.ts) — never from the initial bundle. The lib.d.ts set is
// injected by the caller (globbed raw text in the worker; createDefaultMapFromNodeModules in the node
// test), so the SAME wrap + contract + type-check logic runs in both places.

import ts from 'typescript'
import type { Diagnostic } from '@codemirror/lint'
import {
  createSystem,
  createVirtualTypeScriptEnvironment,
  type VirtualTypeScriptEnvironment,
} from '@typescript/vfs'
import { getLints } from '@valtown/codemirror-ts/worker'
import { FAAS_CONTRACT_DTS } from './contract-dts'
import { CONTRACT_PATH, OPERATOR_PATH, remapDiagnostic, wrapSource } from './ts-wrap'

export { CONTRACT_PATH, OPERATOR_PATH } from './ts-wrap'

// `lib` is intentionally omitted → TypeScript's default lib set for ES2023 (includes DOM, hence
// `typeof fetch`). strict/noImplicitAny are relaxed so partial, mid-typing operator code is not a wall
// of noise; the contract checks that matter (property-not-found 2339, bad-argument 2345, wrong-return
// 2322) fire regardless of strict mode.
export const FAAS_COMPILER_OPTIONS: ts.CompilerOptions = {
  target: ts.ScriptTarget.ES2023,
  module: ts.ModuleKind.ESNext,
  moduleResolution: ts.ModuleResolutionKind.Bundler,
  strict: false,
  noImplicitAny: false,
  skipLibCheck: true,
  noEmit: true,
  allowJs: true,
  checkJs: false,
}

/** Create the virtual TS environment over the caller-supplied lib map + the cap/ctx contract. */
export function createFaasEnv(libFiles: Map<string, string>): VirtualTypeScriptEnvironment {
  const fsMap = new Map(libFiles)
  fsMap.set(CONTRACT_PATH, FAAS_CONTRACT_DTS)
  fsMap.set(OPERATOR_PATH, wrapSource(''))
  const system = createSystem(fsMap)
  return createVirtualTypeScriptEnvironment(
    system,
    [OPERATOR_PATH, CONTRACT_PATH],
    ts,
    FAAS_COMPILER_OPTIONS,
  )
}

/** Wrap `code`, sync it into the env, and return diagnostics mapped back to buffer coordinates. */
export function diagnose(
  env: VirtualTypeScriptEnvironment,
  code: string,
  diagnosticCodesToIgnore: number[] = [],
): Diagnostic[] {
  env.updateFile(OPERATOR_PATH, wrapSource(code))
  const raw = getLints({ env, path: OPERATOR_PATH, diagnosticCodesToIgnore })
  return raw
    .map((d) => remapDiagnostic(d, code.length))
    .filter((d): d is Diagnostic => d !== null)
}
